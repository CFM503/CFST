package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	IPFile         string
	Port           int
	MaxScan        int
	Conc           int
	DownloadNum    int
	Duration       int
	StopThreshold  float64
	Unique         bool
	Output         string
	ScanConcurrent int
	WebPort        string
	WebMode        bool
	URL            string
	Skip429        bool
}

func DefaultConfig() Config {
	return Config{
		Port:           443,
		MaxScan:        2000,
		Conc:           4,
		DownloadNum:    10,
		Duration:       6,
		StopThreshold:  25.0,
		Unique:         false,
		Output:         "result_colo.csv",
		ScanConcurrent: 200,
		WebPort:        "9876",
		URL:            "https://speed.cloudflare.com/__down?bytes=50000000",
		Skip429:        true,
	}
}

func ScanPing(ips []string, port int, concurrency int, progressCallback func(done, total, valid int)) []NodeResult {
	var validNodes []NodeResult
	var mu sync.Mutex
	var done atomic.Int32
	total := len(ips)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, ip := range ips {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()

			lat := TCPPing(ip, port, 1000*time.Millisecond)
			if lat <= 0 {
				time.Sleep(50 * time.Millisecond)
				lat = TCPPing(ip, port, 1000*time.Millisecond)
			}

			d := done.Add(1)
			if lat > 0 {
				mu.Lock()
				validNodes = append(validNodes, NodeResult{IP: ip, Port: port, TCPLatency: lat})
				v := len(validNodes)
				mu.Unlock()
				if progressCallback != nil {
					progressCallback(int(d), total, v)
				}
			} else {
				mu.Lock()
				v := len(validNodes)
				mu.Unlock()
				if progressCallback != nil {
					progressCallback(int(d), total, v)
				}
			}
		}(ip)
	}
	wg.Wait()
	return validNodes
}

func DetectColo(candidates []NodeResult, port int, progressCallback func(done, total int)) {
	var wg sync.WaitGroup
	var done atomic.Int32
	total := len(candidates)
	sem := make(chan struct{}, 20)

	for i := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			candidates[idx].Colo = GetColo(candidates[idx].IP, port)
			d := done.Add(1)
			if progressCallback != nil {
				progressCallback(int(d), total)
			}
		}(i)
	}
	wg.Wait()
}

func RunDownloadTest(candidates []NodeResult, cfg Config, progressRow func(res NodeResult), progressStatus func(msg string), fastExitHost func()) []NodeResult {
	var results []NodeResult
	var fastCount int
	var skipped int

	for i := range candidates {
		msg := fmt.Sprintf("Testing [%d/%d] %s (Skipped: %d)", i+1, len(candidates), candidates[i].IP, skipped)
		if !cfg.WebMode {
			fmt.Printf("\r  --> %-50s", msg)
		}
		if progressStatus != nil {
			progressStatus(msg)
		}

		if CheckBlocked(candidates[i].IP, cfg.Port, cfg.URL) {
			skipped++
			if cfg.Skip429 {
				continue // Silently discard and do not consume a DownloadNum slot
			}
			candidates[i].DownloadSpeed = 0.0
			candidates[i].Colo = "429" // Marked as rate limited
			candidates[i].Score = 0.0
			if !cfg.WebMode {
				fmt.Print("\r                                                               \r")
			}
			if progressRow != nil {
				progressRow(candidates[i])
			}
			results = append(results, candidates[i])
		} else {
			speed := DownloadTest(candidates[i].IP, cfg.Port, cfg.Conc, cfg.Duration, cfg.URL)
			candidates[i].DownloadSpeed = speed
			candidates[i].CalcScore()
			results = append(results, candidates[i])

			if !cfg.WebMode {
				fmt.Print("\r                                                               \r")
			}
			if progressRow != nil {
				progressRow(candidates[i])
			}

			if speed >= cfg.StopThreshold {
				fastCount++
				if fastCount >= 5 {
					if fastExitHost != nil {
						fastExitHost()
					}
					break
				}
			}
		}

		if len(results) >= cfg.DownloadNum {
			break
		}
	}
	if !cfg.WebMode {
		fmt.Print("\r                                                               \r")
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results
}

func RunCLI(cfg Config) {
	fmt.Printf("Cloudflare SpeedTest v1.0.2 (Go Edition)\n\n")

	ips := GenerateIPs(cfg.MaxScan, cfg.Unique, cfg.IPFile)
	fmt.Printf("🔍 Scanning %d IPs (concurrency: %d)...\n", len(ips), cfg.ScanConcurrent)

	validNodes := ScanPing(ips, cfg.Port, cfg.ScanConcurrent, func(done, total, valid int) {
		fmt.Printf("\r  Process: %d/%d | Valid: %d", done, total, valid)
	})
	fmt.Println("\n")

	if len(validNodes) == 0 {
		fmt.Println("[!] No valid IPs found. Please check your network or routing.")
		return
	}

	sort.Slice(validNodes, func(i, j int) bool {
		return validNodes[i].TCPLatency < validNodes[j].TCPLatency
	})

	candidates := validNodes

	fmt.Printf("🌐 Detecting Colo (Top %d)...\n", len(candidates))
	DetectColo(candidates, cfg.Port, nil)

	fmt.Printf("\n🚀 Test Download (%d threads, %ds duration)\n", cfg.Conc, cfg.Duration)
	fmt.Printf("%-16s %-6s %-8s %-20s %-6s\n", "IP", "Colo", "Latency", "Speed", "Score")
	fmt.Println("-----------------------------------------------------------------")

	results := RunDownloadTest(candidates, cfg, func(res NodeResult) {
		if res.Colo != "429" || !cfg.Skip429 {
			fmt.Printf("%-16s %-6s %5.1fms  %5.2f MB/s             %5.1f\n", res.IP, res.Colo, res.TCPLatency, res.DownloadSpeed, res.Score)
		}
	}, nil, func() {
		fmt.Println("\n⚡ Fast-exit triggered.")
	})

	if len(results) == 0 {
		fmt.Println("\n[!] All tested IPs were rate-limited (429/403) by Cloudflare or encountered errors.")
		fmt.Println("[!] Please take a break to let CF clear your IP's rate limit, or use -skip429=false to view skipped IPs.")
		return
	}

	saveCSV(cfg.Output, results)
	fmt.Printf("\n💾 Saved to: %s\n", cfg.Output)
}

func saveCSV(path string, results []NodeResult) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Println("Error saving CSV:", err)
		return
	}
	defer f.Close()

	// UTF-8 BOM
	f.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(f)
	defer w.Flush()

	w.Write([]string{"IP", "Colo", "Latency", "Speed_MB", "Score"})
	for _, r := range results {
		w.Write([]string{
			r.IP,
			r.Colo,
			fmt.Sprintf("%.1f", r.TCPLatency),
			fmt.Sprintf("%.2f", r.DownloadSpeed),
			fmt.Sprintf("%.1f", r.Score),
		})
	}
}
