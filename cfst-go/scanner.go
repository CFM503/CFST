package main

import (
	"encoding/csv"
	"fmt"
	"math"
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
	ColoConcurrent int
	WebPort        string
	WebMode        bool
	URL            string
	Skip429        bool
	YouTubeMode    bool
	Proxy          string
}

func DefaultConfig() Config {
	return Config{
		Port:           443,
		MaxScan:        2000,
		Conc:           4,
		DownloadNum:    10,
		Duration:       15,
		StopThreshold:  25.0,
		Unique:         false,
		Output:         "result_colo.csv",
		ScanConcurrent: 200,
		ColoConcurrent: 20,
		WebPort:        "9876",
		URL:            "https://speed.cloudflare.com/__down?bytes=50000000",
		Skip429:        true,
	}
}

func ScanPing(ips []string, port int, concurrency int, progressCallback func(done, total, valid int)) []NodeResult {
	var validNodes []NodeResult
	var mu sync.Mutex
	var done, validCount atomic.Int32
	total := len(ips)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, ip := range ips {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()

			// 3 次 ping 测量延迟和抖动
			pingCount := 3
			var lats []float64
			for i := 0; i < pingCount; i++ {
				lat := TCPPing(ip, port, 1000*time.Millisecond)
				if lat > 0 {
					lats = append(lats, lat)
				}
				if i < pingCount-1 {
					time.Sleep(30 * time.Millisecond)
				}
			}

			d := done.Add(1)
			if len(lats) > 0 {
				// 平均延迟
				var sum float64
				for _, l := range lats {
					sum += l
				}
				avgLat := sum / float64(len(lats))

				// 抖动（标准差）
				jitter := 0.0
				if len(lats) > 1 {
					var variance float64
					for _, l := range lats {
						diff := l - avgLat
						variance += diff * diff
					}
					variance /= float64(len(lats))
					jitter = math.Sqrt(variance)
				}

				mu.Lock()
				validNodes = append(validNodes, NodeResult{IP: ip, Port: port, TCPLatency: avgLat, Jitter: jitter})
				mu.Unlock()
				validCount.Add(1)
			}
			if progressCallback != nil && (d%10 == 0 || d == int32(total)) {
				progressCallback(int(d), total, int(validCount.Load()))
			}
		}(ip)
	}
	wg.Wait()
	return validNodes
}

func DetectColo(candidates []NodeResult, port int, concurrency int, progressCallback func(done, total int)) {
	var wg sync.WaitGroup
	var done atomic.Int32
	total := len(candidates)
	sem := make(chan struct{}, concurrency)

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
			speed, minSpd, stab := DownloadTest(candidates[i].IP, cfg.Port, cfg.Conc, cfg.Duration, cfg.URL)
			candidates[i].DownloadSpeed = speed
			candidates[i].MinSpeed = minSpd
			candidates[i].Stability = stab
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
	if cfg.YouTubeMode {
		fmt.Printf("YouTube CDN SpeedTest v1.1.1 (Go Edition)\n\n")
	} else {
		fmt.Printf("Cloudflare SpeedTest v1.1.1 (Go Edition)\n\n")
	}

	if cfg.Proxy != "" {
		initProxy(cfg.Proxy)
	}

	var ips []string
	if cfg.YouTubeMode {
		fmt.Printf("🔍 Resolving YouTube CDN nodes (max: %d)...\n", cfg.MaxScan)
		ips = ResolveYouTubeCDNIPs(cfg.MaxScan)
		if len(ips) == 0 {
			fmt.Println("[!] No YouTube CDN IPs found. Please check your network/DNS (YouTube requires proxy in some regions).")
			return
		}
		fmt.Printf("  Found %d unique CDN IPs\n", len(ips))
	} else {
		ips = GenerateIPs(cfg.MaxScan, cfg.Unique, cfg.IPFile)
	}
	fmt.Printf("🔍 Scanning %d IPs (concurrency: %d)...\n", len(ips), cfg.ScanConcurrent)

	validNodes := ScanPing(ips, cfg.Port, cfg.ScanConcurrent, func(done, total, valid int) {
		fmt.Printf("\r  Process: %d/%d | Valid: %d", done, total, valid)
	})
	fmt.Println()

	if len(validNodes) == 0 {
		fmt.Println("[!] No valid IPs found. Please check your network or routing.")
		return
	}

	sort.Slice(validNodes, func(i, j int) bool {
		return validNodes[i].TCPLatency < validNodes[j].TCPLatency
	})

	candidates := validNodes

	fmt.Printf("🌐 Detecting Colo (Top %d)...\n", len(candidates))
	DetectColo(candidates, cfg.Port, cfg.ColoConcurrent, nil)

	fmt.Printf("\n🚀 Test Download (%d threads, %ds duration)\n", cfg.Conc, cfg.Duration)
	fmt.Printf("%-16s %-6s %-8s %-8s %-14s %-8s %-6s\n", "IP", "Colo", "Latency", "Jitter", "Speed", "Stable", "Score")
	fmt.Println("---------------------------------------------------------------------------------")

	results := RunDownloadTest(candidates, cfg, func(res NodeResult) {
		if res.Colo != "429" || !cfg.Skip429 {
			fmt.Printf("%-16s %-6s %5.1fms  %5.1fms  %5.2f MB/s    %4.0f%%   %5.1f\n", res.IP, res.Colo, res.TCPLatency, res.Jitter, res.DownloadSpeed, res.Stability, res.Score)
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

	w.Write([]string{"IP", "Colo", "Latency", "Jitter", "Speed_MB", "MinSpeed_MB", "Stability", "Score"})
	for _, r := range results {
		w.Write([]string{
			r.IP,
			r.Colo,
			fmt.Sprintf("%.1f", r.TCPLatency),
			fmt.Sprintf("%.1f", r.Jitter),
			fmt.Sprintf("%.2f", r.DownloadSpeed),
			fmt.Sprintf("%.2f", r.MinSpeed),
			fmt.Sprintf("%.0f", r.Stability),
			fmt.Sprintf("%.1f", r.Score),
		})
	}
}
