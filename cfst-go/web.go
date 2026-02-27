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
		if c := q.Get("c"); c != "" {
			reqCfg.Conc, _ = strconv.Atoi(c)
		}
		if dt := q.Get("dt"); dt != "" {
			reqCfg.Duration, _ = strconv.Atoi(dt)
		}
		if u := q.Get("url"); u != "" {
			reqCfg.URL = u
		}
		if s := q.Get("skip429"); s != "" {
			reqCfg.Skip429 = (s == "true")
		}

		var sendMu sync.Mutex
		sendEvent := func(evtType string, data interface{}) {
			sendMu.Lock()
			defer sendMu.Unlock()
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evtType, string(b))
			flusher.Flush()
		}

		sendEvent("status", "Generating IP Ranges...")
		ips := GenerateIPs(reqCfg.MaxScan, reqCfg.Unique, reqCfg.IPFile)

		sendEvent("status", fmt.Sprintf("Ping Scanning %d IPs...", len(ips)))
		validNodes := ScanPing(ips, reqCfg.Port, reqCfg.ScanConcurrent, func(done, total, valid int) {
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

		sendEvent("status", fmt.Sprintf("Detecting Colo for %d nodes...", len(candidates)))
		DetectColo(candidates, reqCfg.Port, func(done, total int) {
			if done%5 == 0 || done == total {
				sendEvent("progress_colo", map[string]int{"done": done, "total": total})
			}
		})

		sendEvent("status", "Running Download Speed Tests...")
		results := RunDownloadTest(candidates, reqCfg, func(res NodeResult) {
			if res.Colo != "429" || !reqCfg.Skip429 {
				sendEvent("progress_download", res)
			}
		}, func(msg string) {
			sendEvent("status", msg)
		}, func() {
			sendEvent("fast_exit", "Target speed threshold reached, stopping early.")
		})

		if len(results) == 0 {
			sendEvent("error", "All tested IPs were rate-limited (429/403) by Cloudflare. Please wait or change the URL.")
			return
		}

		sendEvent("status", "Test Complete")
		sendEvent("complete", "done")
	})

	fmt.Printf("🚀 Web UI started. Open http://localhost%s in your browser\n", cfg.WebPort)
	err := http.ListenAndServe(cfg.WebPort, nil)
	if err != nil {
		fmt.Printf("Web server error: %v\n", err)
	}
}
