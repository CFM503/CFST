package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	IPFile         string
	Port           int
	MaxScan        int
	TopN           int     // 从延迟排序候选中取前 N 个用于测速
	DLConc         int     // 并行下载测试并发数
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
	YouTubeMode    bool
	Proxy          string
	QuickDuration  int     // 自定义 URL 时快速筛选测试时长（秒）
}

func DefaultConfig() Config {
	return Config{
		Port:           443,
		MaxScan:        5000,
		TopN:           100,
		DLConc:         3,
		DownloadNum:    10,
		Duration:       15,
		StopThreshold:  15.0,
		Unique:         false,
		Output:         "result_colo.csv",
		ScanConcurrent: 200,
		WebPort:        "9876",
		URL:            "https://speed.cloudflare.com/__down?bytes=500000000",
		Skip429:        true,
		QuickDuration:  3,
	}
}

// isCustomURL returns true if the URL is not the default speed.cloudflare.com endpoint.
func isCustomURL(urlStr string) bool {
	return !strings.Contains(urlStr, "speed.cloudflare.com/__down")
}

func ScanPing(ctx context.Context, ips []string, port int, concurrency int, progressCallback func(done, total, valid int)) []NodeResult {
	var validNodes []NodeResult
	var mu sync.Mutex
	var done, validCount atomic.Int32
	total := len(ips)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Done()
			continue
		}

		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}

			// 5 次 ping 测量延迟和抖动，网络不稳时更客观
			pingCount := 5
			lats := make([]float64, 0, 5)
			for i := 0; i < pingCount; i++ {
				if ctx.Err() != nil {
					return
				}
				lat := TCPPing(ip, port, 1500*time.Millisecond)
				if lat > 0 {
					lats = append(lats, lat)
				}
				if i < pingCount-1 {
					select {
					case <-time.After(30 * time.Millisecond):
					case <-ctx.Done():
						return
					}
				}
			}

			d := done.Add(1)
			// 丢包率过滤：5 次 ping 中至少 4 次成功才保留
			minSuccess := pingCount - 1 // 允许丢 1 包
			if len(lats) >= minSuccess {
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

				loss := float64(pingCount-len(lats)) / float64(pingCount)
				mu.Lock()
				validNodes = append(validNodes, NodeResult{IP: ip, Port: port, TCPLatency: avgLat, Jitter: jitter, PacketLoss: loss})
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

// detectColoBatch 并发检测所有候选节点的 Colo，按 Colo 分组返回最优组
func detectColoBatch(ctx context.Context, candidates []NodeResult, port int, concurrency int, proxyAddr string, progressCallback func(done, total int)) (bestColo string, coloGroups map[string][]NodeResult) {
	var wg sync.WaitGroup
	var done atomic.Int32
	total := len(candidates)
	sem := make(chan struct{}, concurrency)

	for i := range candidates {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Done()
			continue
		}

		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			candidates[idx].Colo = GetColo(candidates[idx].IP, port, proxyAddr)
			d := done.Add(1)
			if progressCallback != nil && (d%20 == 0 || d == int32(total)) {
				progressCallback(int(d), total)
			}
		}(i)
	}
	wg.Wait()

	// 按 Colo 分组，选择节点数最多的 Colo
	coloGroups = make(map[string][]NodeResult)
	for _, c := range candidates {
		if c.Colo != "ERR" && c.Colo != "UNK" && c.Colo != "" {
			coloGroups[c.Colo] = append(coloGroups[c.Colo], c)
		}
	}

	bestColo = ""
	bestCount := 0
	for colo, nodes := range coloGroups {
		if len(nodes) > bestCount {
			bestCount = len(nodes)
			bestColo = colo
		}
	}
	return bestColo, coloGroups
}

// runQuickFilter 快速下载筛选：对所有候选 IP 做短时下载测试，按速度排序取 TopN
// 用于自定义 URL 场景，替代延迟 TopN 筛选
func runQuickFilter(ctx context.Context, candidates []NodeResult, cfg Config, topN int,
	progressCallback func(done, total int)) []NodeResult {

	numWorkers := cfg.DLConc
	if numWorkers < 1 {
		numWorkers = 1
	}

	type quickResult struct {
		idx   int
		speed float64
	}

	results := make([]quickResult, len(candidates))
	var doneCount atomic.Int32

	sem := make(chan struct{}, numWorkers)
	var wg sync.WaitGroup

	for i, cand := range candidates {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, ip string) {
			defer wg.Done()
			defer func() { <-sem }()

			speed, _, _ := SingleStreamTest(ctx, ip, cfg.Port, cfg.QuickDuration, cfg.URL, cfg.Proxy, nil)
			results[idx] = quickResult{idx: idx, speed: speed}

			d := doneCount.Add(1)
			if progressCallback != nil {
				progressCallback(int(d), len(candidates))
			}
		}(i, cand.IP)
	}

	wg.Wait()

	// 按速度降序排序
	sort.Slice(results, func(i, j int) bool {
		return results[i].speed > results[j].speed
	})

	// 取 TopN 有速度的候选
	var filtered []NodeResult
	for _, r := range results {
		if r.speed <= 0 {
			continue
		}
		filtered = append(filtered, candidates[r.idx])
		if len(filtered) >= topN {
			break
		}
	}
	return filtered
}

// runParallelDownloadTest 并行下载测试，多个 worker 同时测速
func runParallelDownloadTest(ctx context.Context, candidates []NodeResult, cfg Config,
	progressRow func(res NodeResult),
	progressStatus func(msg string),
	progressLive func(LiveProgress),
	fastExitHost func()) []NodeResult {

	numWorkers := cfg.DLConc
	if numWorkers < 1 {
		numWorkers = 1
	}
	if numWorkers > len(candidates) {
		numWorkers = len(candidates)
	}

	var results []NodeResult
	var mu sync.Mutex
	var fastCount atomic.Int32
	var totalTested atomic.Int32
	var totalSkipped atomic.Int32

	resultCh := make(chan NodeResult, numWorkers*2)
	doneCh := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(doneCh) }) }

	// 收集结果的 goroutine
	go func() {
		for res := range resultCh {
			mu.Lock()
			results = append(results, res)
			n := len(results)
			mu.Unlock()

			if progressRow != nil {
				progressRow(res)
			}
			if n >= cfg.DownloadNum {
				closeDone()
				return
			}
		}
		closeDone()
	}()

	// 启动 workers
	var wg sync.WaitGroup
	var nextIdx atomic.Int32
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workerCooldownMs := 500

			for {
				// 检查是否已收集够结果
				select {
				case <-doneCh:
					return
				case <-ctx.Done():
					return
				default:
				}

				// 获取下一个候选 IP
				idx := int(nextIdx.Add(1) - 1)
				if idx >= len(candidates) {
					return
				}
				cand := candidates[idx]

				// worker 间冷却
				if workerCooldownMs > 0 && idx > 0 {
					select {
					case <-time.After(time.Duration(workerCooldownMs) * time.Millisecond):
					case <-ctx.Done():
						return
					case <-doneCh:
						return
					}
				}

				// 检查 fast-exit
				if fastCount.Load() >= 5 {
					return
				}

				testURL := cfg.URL
				if cand.TestURL != "" {
					testURL = cand.TestURL
				}

				t := totalTested.Add(1)
				msg := fmt.Sprintf("Testing [%d/%d] %s (Skipped: %d)", t, len(candidates), cand.IP, int(totalSkipped.Load()))
				if progressStatus != nil {
					progressStatus(msg)
				}

				speed, minSpd, stab := SingleStreamTest(ctx, cand.IP, cfg.Port, cfg.Duration, testURL, cfg.Proxy, progressLive)

				if speed == 0 && minSpd == 0 && stab == 0 {
					totalSkipped.Add(1)
					workerCooldownMs = min(workerCooldownMs*2, 5000)
					if cfg.Skip429 {
						continue
					}
					cand.DownloadSpeed = 0
					cand.Colo = "429"
					cand.Score = 0
					select {
					case resultCh <- cand:
					case <-doneCh:
						return
					}
				} else {
					workerCooldownMs = 500

					// YouTube 节点不走 Cloudflare，跳过 Colo 和 LoadLatency 检测
					if cand.Domain != "" {
						cand.Colo = "YT"
					} else {
						cand.Colo = GetColo(cand.IP, cfg.Port, cfg.Proxy)
						cand.LoadLatency = MeasureLoadLatency(cand.IP, cfg.Port, cfg.Proxy)
					}
					cand.DownloadSpeed = speed
					cand.SingleSpeed = speed
					cand.MinSpeed = minSpd
					cand.Stability = stab
					cand.CalcScore()

					select {
					case resultCh <- cand:
					case <-doneCh:
						return
					}

					if speed >= cfg.StopThreshold {
						if fastCount.Add(1) >= 5 {
							if fastExitHost != nil {
								fastExitHost()
							}
							return
						}
					}
				}
			}
		}()
	}

	wg.Wait()
	close(resultCh)
	<-doneCh

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results
}

func RunCLI(cfg Config) {
	if cfg.YouTubeMode {
		fmt.Printf("YouTube CDN SpeedTest v1.7.4 (Go Edition)\n\n")
	} else {
		fmt.Printf("Cloudflare SpeedTest v1.7.4 (Go Edition)\n\n")
	}

	var ips []string
	var ytNodeMap map[string]NodeResult // IP -> NodeResult with TestURL/Domain
	if cfg.YouTubeMode {
		fmt.Printf("🔍 Resolving YouTube CDN nodes (max: %d)...\n", cfg.MaxScan)
		ytNodes := ResolveYouTubeCDNIPs(cfg.MaxScan, cfg.Proxy)
		if len(ytNodes) == 0 {
			fmt.Println("[!] No YouTube CDN IPs found. Please check your network/DNS (YouTube requires proxy in some regions).")
			return
		}
		ytNodeMap = make(map[string]NodeResult, len(ytNodes))
		for _, n := range ytNodes {
			ips = append(ips, n.IP)
			ytNodeMap[n.IP] = n
		}
		fmt.Printf("  Found %d unique CDN IPs\n", len(ips))
	} else {
		ips = GenerateIPs(cfg.MaxScan, cfg.Unique, cfg.IPFile)
	}
	fmt.Printf("🔍 Scanning %d IPs (concurrency: %d)...\n", len(ips), cfg.ScanConcurrent)

	ctx := context.Background()

	validNodes := ScanPing(ctx, ips, cfg.Port, cfg.ScanConcurrent, func(done, total, valid int) {
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

	// For YouTube mode, inject TestURL/Domain into candidates
	if ytNodeMap != nil {
		for i := range candidates {
			if yt, ok := ytNodeMap[candidates[i].IP]; ok {
				candidates[i].TestURL = yt.TestURL
				candidates[i].Domain = yt.Domain
			}
		}
	}

	// 自定义 URL 时：用快速下载测试筛选（延迟不等于到 VPS 的下载速度）
	// 默认 speed.cloudflare.com 时：用延迟 TopN 筛选（延迟是好的代理指标）
	if isCustomURL(cfg.URL) && !cfg.YouTubeMode {
		fmt.Printf("\n⚡ Quick filter: %ds download test for %d candidates (concurrency: %d)...\n", cfg.QuickDuration, len(candidates), cfg.DLConc)
		candidates = runQuickFilter(ctx, candidates, cfg, cfg.TopN, func(done, total int) {
			fmt.Printf("\r  Quick filter: %d/%d", done, total)
		})
		fmt.Println()
		if len(candidates) == 0 {
			fmt.Println("[!] No IPs with measurable download speed. Check your URL or network.")
			return
		}
		fmt.Printf("  ✓ %d candidates passed quick filter\n", len(candidates))
	} else {
		// 取延迟最低的 TopN 个候选
		if len(candidates) > cfg.TopN {
			candidates = candidates[:cfg.TopN]
		}
	}

	// Step 2: Colo 检测（YouTube 模式跳过，googlevideo.com 不是 Cloudflare）
	if !cfg.YouTubeMode {
		fmt.Printf("\n🔍 Detecting Colo for %d candidates...\n", len(candidates))
		bestColo, coloGroups := detectColoBatch(ctx, candidates, cfg.Port, cfg.ScanConcurrent, cfg.Proxy, func(done, total int) {
			fmt.Printf("\r  Colo detection: %d/%d", done, total)
		})
		fmt.Println()

		if bestColo != "" {
			fmt.Printf("  Best Colo: %s (%d nodes)\n", bestColo, len(coloGroups[bestColo]))
			type coloStat struct {
				name  string
				count int
				avgMs float64
			}
			var stats []coloStat
			for colo, nodes := range coloGroups {
				var sum float64
				for _, n := range nodes {
					sum += n.TCPLatency
				}
				stats = append(stats, coloStat{colo, len(nodes), sum / float64(len(nodes))})
			}
			sort.Slice(stats, func(i, j int) bool { return stats[i].count > stats[j].count })
			for _, s := range stats {
				marker := "  "
				if s.name == bestColo {
					marker = "★ "
				}
				fmt.Printf("  %s%-6s  %4d nodes  avg %.1fms\n", marker, s.name, s.count, s.avgMs)
			}
			candidates = coloGroups[bestColo]
		} else {
			fmt.Println("  [!] No valid Colo detected, testing all candidates")
		}
	} else {
		fmt.Printf("\n[YouTube Mode] Skipping Colo detection, testing %d candidates directly...\n", len(candidates))
	}

	// Step 3: 并行下载测试
	fmt.Printf("\n🚀 Download Test (%ds duration, %d parallel)\n", cfg.Duration, cfg.DLConc)
	fmt.Printf("%-16s %-6s %-8s %-8s %-14s %-12s %-8s %-8s %-6s\n", "IP", "Colo", "Latency", "Jitter", "Speed", "MinSpd", "LoadLat", "Stable", "Score")
	fmt.Println("------------------------------------------------------------------------------------")

	results := runParallelDownloadTest(ctx, candidates, cfg, func(res NodeResult) {
		if res.Colo != "429" || !cfg.Skip429 {
			fmt.Printf("\r%-120s\r", "") // 清除实时进度行
			fmt.Printf("%-16s %-6s %5.1fms  %5.1fms  %6.2f MB/s  %5.2f MB/s  %5.1fms  %4.0f%%   %5.1f\n", res.IP, res.Colo, res.TCPLatency, res.Jitter, res.DownloadSpeed, res.MinSpeed, res.LoadLatency, res.Stability, res.Score)
		}
	}, nil, func(p LiveProgress) {
		fmt.Printf("\r  📥 %-16s %6.1f MB  %6.2f MB/s  %4.0f/%ds    ", p.IP, float64(p.Bytes)/1024.0/1024.0, p.Speed, p.Elapsed, int(p.Duration))
	}, func() {
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

	w.Write([]string{"IP", "Colo", "Latency", "Jitter", "SgSpeed_MB", "Speed_MB", "MinSpeed_MB", "LoadLatency", "Stability", "Score"})
	for _, r := range results {
		w.Write([]string{
			r.IP,
			r.Colo,
			fmt.Sprintf("%.1f", r.TCPLatency),
			fmt.Sprintf("%.1f", r.Jitter),
			fmt.Sprintf("%.2f", r.SingleSpeed),
			fmt.Sprintf("%.2f", r.DownloadSpeed),
			fmt.Sprintf("%.2f", r.MinSpeed),
			fmt.Sprintf("%.1f", r.LoadLatency),
			fmt.Sprintf("%.0f", r.Stability),
			fmt.Sprintf("%.1f", r.Score),
		})
	}
}
