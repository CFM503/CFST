package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
)

//go:embed index.html
var indexHTML []byte

func RunWeb(cfg Config) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	http.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		reqCfg := cfg
		q := r.URL.Query()
		if m := q.Get("max"); m != "" {
			reqCfg.MaxScan, _ = strconv.Atoi(m)
		}
		if p := q.Get("port"); p != "" {
			reqCfg.Port, _ = strconv.Atoi(p)
		}
		if dn := q.Get("dn"); dn != "" {
			reqCfg.DownloadNum, _ = strconv.Atoi(dn)
		}
		if t := q.Get("topn"); t != "" {
			reqCfg.TopN, _ = strconv.Atoi(t)
		}
		if d := q.Get("dlc"); d != "" {
			reqCfg.DLConc, _ = strconv.Atoi(d)
		}
		if dt := q.Get("dt"); dt != "" {
			reqCfg.Duration, _ = strconv.Atoi(dt)
		}
		if u := q.Get("url"); u != "" {
			reqCfg.URL = u
		}
		if qd := q.Get("qd"); qd != "" {
			reqCfg.QuickDuration, _ = strconv.Atoi(qd)
		}
		if s := q.Get("skip429"); s != "" {
			reqCfg.Skip429 = (s == "true")
		}
		if f := q.Get("filter"); f != "" {
			reqCfg.FilterMode = f
		}

		var sendMu sync.Mutex
		sendEvent := func(evtType string, data interface{}) {
			sendMu.Lock()
			defer sendMu.Unlock()
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: ", evtType)
			w.Write(b)
			fmt.Fprint(w, "\n\n")
			flusher.Flush()
		}

		sendEvent("status", "Generating IPs...")
		ips := GenerateIPs(reqCfg.MaxScan, reqCfg.Unique, reqCfg.IPFile)

		sendEvent("status", fmt.Sprintf("Ping scanning %d IPs...", len(ips)))
		validNodes := ScanPing(r.Context(), ips, reqCfg.Port, reqCfg.ScanConcurrent, func(done, total, valid int) {
			if done%10 == 0 || done == total {
				sendEvent("progress_scan", map[string]int{"done": done, "total": total, "valid": valid})
			}
		})

		if len(validNodes) == 0 {
			sendEvent("error", "No valid IPs found.")
			return
		}

		sort.Slice(validNodes, func(i, j int) bool {
			return validNodes[i].TCPLatency < validNodes[j].TCPLatency
		})
		candidates := validNodes

		if isCustomURL(reqCfg.URL) {
			reqCfg.SkipLoadLatency = true
			reqCfg.StopThreshold = 9999.0 // disable fast-exit
			if reqCfg.FilterMode == "multi-colo" {
				sendEvent("status", "Multi-Colo filtering not supported in custom URL mode. Falling back to speed pre-filter...")
				reqCfg.FilterMode = "speed"
			}
		}

		switch reqCfg.FilterMode {
		case "speed":
			// Cap pre-filter pool to TopN*2 (sorted by latency), boost concurrency.
			quickPool := candidates
			maxPool := reqCfg.TopN * 2
			if len(quickPool) > maxPool {
				quickPool = quickPool[:maxPool]
			}
			quickCfg := reqCfg
			quickCfg.DLConc = reqCfg.DLConc * 3
			if quickCfg.DLConc < 6 {
				quickCfg.DLConc = 6
			}

			sendEvent("status", fmt.Sprintf("Speed Pre-filter: running quick test (%ds) on %d candidates (%d workers)...",
				reqCfg.QuickDuration, len(quickPool), quickCfg.DLConc))
			candidates = runQuickFilter(r.Context(), quickPool, quickCfg, reqCfg.TopN, func(done, total int) {
				sendEvent("progress_colo", map[string]int{"done": done, "total": total})
			})

		case "multi-colo":
			if len(candidates) > reqCfg.TopN {
				candidates = candidates[:reqCfg.TopN]
			}

			sendEvent("status", fmt.Sprintf("Detecting Colo for %d candidates...", len(candidates)))
			_, coloGroups := detectColoBatch(r.Context(), candidates, reqCfg.Port, reqCfg.ScanConcurrent, func(done, total int) {
				sendEvent("progress_colo", map[string]int{"done": done, "total": total})
			})

			if len(coloGroups) > 0 {
				type coloStat struct {
					name  string
					count int
					avgMs float64
				}
				var stats []coloStat
				for colo, nodes := range coloGroups {
					stats = append(stats, coloStat{colo, len(nodes), avgLatency(nodes)})
				}
				sort.Slice(stats, func(i, j int) bool { return stats[i].avgMs < stats[j].avgMs })

				var multiColoCandidates []NodeResult
				numColos := 3
				if len(stats) < numColos {
					numColos = len(stats)
				}
				for i := 0; i < numColos; i++ {
					multiColoCandidates = append(multiColoCandidates, coloGroups[stats[i].name]...)
				}
				candidates = multiColoCandidates
				sendEvent("status", fmt.Sprintf("Selected top %d Colos (candidates: %d) — running download test...", numColos, len(candidates)))
			} else {
				sendEvent("status", "No valid Colo detected, testing all candidates...")
			}

		default: // "none"
			if len(candidates) > reqCfg.TopN {
				candidates = candidates[:reqCfg.TopN]
			}
			sendEvent("status", fmt.Sprintf("Skipping candidate filtering, testing top %d candidates directly...", len(candidates)))
		}

		if len(candidates) == 0 {
			sendEvent("error", "No candidates selected for testing.")
			return
		}

		results := runParallelDownloadTest(r.Context(), candidates, reqCfg, func(res NodeResult) {
			if res.Colo != "429" || !reqCfg.Skip429 {
				sendEvent("progress_download", res)
			}
		}, func(msg string) {
			sendEvent("status", msg)
		}, func(p LiveProgress) {
			sendEvent("progress_live", p)
		}, func() {
			sendEvent("fast_exit", "Speed threshold reached, stopping early.")
		})

		if len(results) == 0 {
			sendEvent("error", "All tested IPs failed or were rate-limited. Please wait and retry.")
			return
		}
		sendEvent("status", "Test Complete")
		sendEvent("complete", results)
	})

	fmt.Printf("🚀 Web UI started. Open http://localhost%s in your browser\n", cfg.WebPort)
	if err := http.ListenAndServe(cfg.WebPort, nil); err != nil {
		fmt.Printf("Web server error: %v\n", err)
	}
}
