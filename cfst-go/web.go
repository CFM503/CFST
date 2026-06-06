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
		if s := q.Get("yt"); s == "true" {
			reqCfg.YouTubeMode = true
		}
		if p := q.Get("proxy"); p != "" {
			reqCfg.Proxy = p
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

		var ips []string
		var ytNodeMap map[string]NodeResult
		if reqCfg.YouTubeMode {
			sendEvent("status", "Resolving YouTube CDN nodes...")
			ytNodes := ResolveYouTubeCDNIPs(reqCfg.MaxScan, reqCfg.Proxy)
			if len(ytNodes) == 0 {
				sendEvent("error", "No YouTube CDN IPs found. Check network/DNS (YouTube requires proxy in some regions).")
				return
			}
			ytNodeMap = make(map[string]NodeResult, len(ytNodes))
			for _, n := range ytNodes {
				ips = append(ips, n.IP)
				ytNodeMap[n.IP] = n
			}
			sendEvent("status", fmt.Sprintf("Found %d YouTube CDN IPs", len(ips)))
		} else {
			sendEvent("status", "Generating IP Ranges...")
			ips = GenerateIPs(reqCfg.MaxScan, reqCfg.Unique, reqCfg.IPFile)
		}

		sendEvent("status", fmt.Sprintf("Ping Scanning %d IPs...", len(ips)))
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

		// For YouTube mode, inject TestURL/Domain into candidates
		if ytNodeMap != nil {
			for i := range candidates {
				if yt, ok := ytNodeMap[candidates[i].IP]; ok {
					candidates[i].TestURL = yt.TestURL
					candidates[i].Domain = yt.Domain
				}
			}
		}

		// 自定义 URL 时用快速下载筛选，否则用延迟 TopN
		if isCustomURL(reqCfg.URL) && !reqCfg.YouTubeMode {
			sendEvent("status", fmt.Sprintf("Quick filter: %ds download test for %d candidates...", reqCfg.QuickDuration, len(candidates)))
			candidates = runQuickFilter(r.Context(), candidates, reqCfg, reqCfg.TopN, func(done, total int) {
				sendEvent("progress_quick", map[string]int{"done": done, "total": total})
			})
			if len(candidates) == 0 {
				sendEvent("error", "No IPs with measurable download speed. Check your URL or network.")
				return
			}
			sendEvent("status", fmt.Sprintf("✓ %d candidates passed quick filter", len(candidates)))
		} else {
			if len(candidates) > reqCfg.TopN {
				candidates = candidates[:reqCfg.TopN]
			}
		}

		// Colo 检测（YouTube 模式跳过）
		if !reqCfg.YouTubeMode {
			sendEvent("status", fmt.Sprintf("Detecting Colo for %d candidates...", len(candidates)))
			bestColo, coloGroups := detectColoBatch(r.Context(), candidates, reqCfg.Port, reqCfg.ScanConcurrent, reqCfg.Proxy, func(done, total int) {
				sendEvent("progress_colo", map[string]int{"done": done, "total": total})
			})
			if bestColo != "" {
				sendEvent("status", fmt.Sprintf("Best Colo: %s (%d nodes), testing...", bestColo, len(coloGroups[bestColo])))
				candidates = coloGroups[bestColo]
			} else {
				sendEvent("status", "No valid Colo detected, testing all candidates...")
			}
		} else {
			sendEvent("status", fmt.Sprintf("[YouTube Mode] Testing %d candidates directly...", len(candidates)))
		}

		sendEvent("status", "Running Download Speed Tests...")
		results := runParallelDownloadTest(r.Context(), candidates, reqCfg, func(res NodeResult) {
			if res.Colo != "429" || !reqCfg.Skip429 {
				sendEvent("progress_download", res)
			}
		}, func(msg string) {
			sendEvent("status", msg)
		}, func(p LiveProgress) {
			sendEvent("progress_live", p)
		}, func() {
			sendEvent("fast_exit", "Target speed threshold reached, stopping early.")
		})

		if len(results) == 0 {
			sendEvent("error", "All tested IPs were rate-limited (429/403) by Cloudflare. Please wait or change the URL.")
			return
		}

		sendEvent("status", "Test Complete")
		sendEvent("complete", results)
	})

	fmt.Printf("🚀 Web UI started. Open http://localhost%s in your browser\n", cfg.WebPort)
	err := http.ListenAndServe(cfg.WebPort, nil)
	if err != nil {
		fmt.Printf("Web server error: %v\n", err)
	}
}
