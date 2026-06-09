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
	IPFile          string
	Port            int
	MaxScan         int
	TopN            int
	DLConc          int
	DownloadNum     int
	Duration        int
	StopThreshold   float64
	Unique          bool
	Output          string
	ScanConcurrent  int
	WebPort         string
	WebMode         bool
	URL             string
	Skip429         bool
	QuickDuration   int
	SkipLoadLatency bool // auto-set for custom URL mode
	FilterMode      string
}

func DefaultConfig() Config {
	return Config{
		Port:           443,
		MaxScan:        3000,
		TopN:           100,
		DLConc:         1,
		DownloadNum:    20,
		Duration:       20,
		StopThreshold:  30.0,
		Unique:         false,
		Output:         "result_colo.csv",
		ScanConcurrent: 200,
		WebPort:        "9876",
		URL:            "https://speed.cloudflare.com/__down?bytes=500000000",
		Skip429:        true,
		QuickDuration:  3,
		FilterMode:     "speed",
	}
}

func isCustomURL(urlStr string) bool {
	return !strings.Contains(urlStr, "speed.cloudflare.com/__down")
}

// ScanPing runs 5 TCP pings per IP and filters by packet loss.
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
			if len(lats) >= pingCount-1 { // allow 1 packet loss
				var sum float64
				for _, l := range lats {
					sum += l
				}
				avgLat := sum / float64(len(lats))

				jitter := 0.0
				if len(lats) > 1 {
					var variance float64
					for _, l := range lats {
						diff := l - avgLat
						variance += diff * diff
					}
					jitter = math.Sqrt(variance / float64(len(lats)))
				}

				loss := float64(pingCount-len(lats)) / float64(pingCount)
				mu.Lock()
				validNodes = append(validNodes, NodeResult{
					IP: ip, Port: port,
					TCPLatency: avgLat, Jitter: jitter, PacketLoss: loss,
				})
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

// avgLatency returns the average TCPLatency of a node slice.
func avgLatency(nodes []NodeResult) float64 {
	if len(nodes) == 0 {
		return math.MaxFloat64
	}
	var sum float64
	for _, n := range nodes {
		sum += n.TCPLatency
	}
	return sum / float64(len(nodes))
}

// selectBestColo picks the Colo with the LOWEST average TCP latency.
// This is the correct criterion — not node count.
func selectBestColo(coloGroups map[string][]NodeResult) string {
	const minNodes = 3
	best := ""
	bestLat := math.MaxFloat64

	// First pass: colos with enough nodes for statistical confidence
	for colo, nodes := range coloGroups {
		if len(nodes) < minNodes {
			continue
		}
		if avg := avgLatency(nodes); avg < bestLat {
			bestLat = avg
			best = colo
		}
	}
	// Fallback: any colo
	if best == "" {
		for colo, nodes := range coloGroups {
			if avg := avgLatency(nodes); avg < bestLat {
				bestLat = avg
				best = colo
			}
		}
	}
	return best
}

// detectColoBatch concurrently queries the Colo for each candidate.
// Returns the best Colo (by lowest avg latency) and the full coloGroups map.
func detectColoBatch(ctx context.Context, candidates []NodeResult, port int, concurrency int,
	progressCallback func(done, total int)) (bestColo string, coloGroups map[string][]NodeResult) {

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
			candidates[idx].Colo = GetColo(candidates[idx].IP, port)
			d := done.Add(1)
			if progressCallback != nil && (d%20 == 0 || d == int32(total)) {
				progressCallback(int(d), total)
			}
		}(i)
	}
	wg.Wait()

	coloGroups = make(map[string][]NodeResult)
	for _, c := range candidates {
		if c.Colo != "ERR" && c.Colo != "UNK" && c.Colo != "" {
			coloGroups[c.Colo] = append(coloGroups[c.Colo], c)
		}
	}

	bestColo = selectBestColo(coloGroups)
	return bestColo, coloGroups
}

// runQuickFilter runs short download tests against cfg.URL to rank candidates by speed.
// Used as a pre-filter in custom URL mode instead of Colo detection.
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
			speed, _, _ := SingleStreamTest(ctx, ip, cfg.Port, cfg.QuickDuration, cfg.URL, nil)
			results[idx] = quickResult{idx: idx, speed: speed}
			d := doneCount.Add(1)
			if progressCallback != nil {
				progressCallback(int(d), len(candidates))
			}
		}(i, cand.IP)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		return results[i].speed > results[j].speed
	})

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

// runParallelDownloadTest runs the full download test on candidates.
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

	var wg sync.WaitGroup
	var nextIdx atomic.Int32
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workerCooldownMs := 500

			for {
				select {
				case <-doneCh:
					return
				case <-ctx.Done():
					return
				default:
				}

				idx := int(nextIdx.Add(1) - 1)
				if idx >= len(candidates) {
					return
				}
				cand := candidates[idx]

				if workerCooldownMs > 0 && idx > 0 {
					select {
					case <-time.After(time.Duration(workerCooldownMs) * time.Millisecond):
					case <-ctx.Done():
						return
					case <-doneCh:
						return
					}
				}

				if fastCount.Load() >= 5 {
					return
				}

				t := totalTested.Add(1)
				if progressStatus != nil {
					progressStatus(fmt.Sprintf("Testing [%d/%d] %s (Skipped: %d)",
						t, len(candidates), cand.IP, int(totalSkipped.Load())))
				}

				speed, minSpd, stab := SingleStreamTest(ctx, cand.IP, cfg.Port, cfg.Duration, cfg.URL, progressLive)

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
					cand.Colo = GetColo(cand.IP, cfg.Port)
					if !cfg.SkipLoadLatency {
						cand.LoadLatency = MeasureLoadLatency(cand.IP, cfg.Port)
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
	fmt.Printf("Cloudflare SpeedTest v1.8.2 (Go Edition)\n\n")

	ips := GenerateIPs(cfg.MaxScan, cfg.Unique, cfg.IPFile)
	fmt.Printf("🔍 Scanning %d IPs (concurrency: %d)...\n", len(ips), cfg.ScanConcurrent)

	ctx := context.Background()

	validNodes := ScanPing(ctx, ips, cfg.Port, cfg.ScanConcurrent, func(done, total, valid int) {
		fmt.Printf("\r  Process: %d/%d | Valid: %d", done, total, valid)
	})
	fmt.Println()

	if len(validNodes) == 0 {
		fmt.Println("[!] No valid IPs found.")
		return
	}

	sort.Slice(validNodes, func(i, j int) bool {
		return validNodes[i].TCPLatency < validNodes[j].TCPLatency
	})

	candidates := validNodes

	if isCustomURL(cfg.URL) {
		cfg.SkipLoadLatency = true
		cfg.StopThreshold = 9999.0 // disable fast-exit
		if cfg.FilterMode == "multi-colo" {
			fmt.Println("[!] Multi-Colo filtering is not supported in custom URL mode. Falling back to speed pre-filter.")
			cfg.FilterMode = "speed"
		}
	}

	switch cfg.FilterMode {
	case "speed":
		// Cap quick filter pool: take top TopN*2 by latency (already sorted).
		// This bounds pre-filter time to ~1-2 min regardless of total candidates.
		quickPool := candidates
		maxPool := cfg.TopN * 2
		if len(quickPool) > maxPool {
			quickPool = quickPool[:maxPool]
		}
		// Boost concurrency for the rough pre-filter pass (parallel is fine here).
		quickCfg := cfg
		quickCfg.DLConc = cfg.DLConc * 3
		if quickCfg.DLConc < 6 {
			quickCfg.DLConc = 6
		}

		fmt.Printf("\n⚡ Speed Pre-filter mode (%ds quick test on %d candidates, %d workers)...\n",
			cfg.QuickDuration, len(quickPool), quickCfg.DLConc)

		candidates = runQuickFilter(ctx, quickPool, quickCfg, cfg.TopN, func(d, t int) {
			fmt.Printf("\r  Pre-filter: %d/%d", d, t)
		})
		fmt.Printf("\n  → %d candidates selected for full test\n", len(candidates))

	case "multi-colo":
		if len(candidates) > cfg.TopN {
			candidates = candidates[:cfg.TopN]
		}

		fmt.Printf("\n🔍 Detecting Colo for %d candidates...\n", len(candidates))
		_, coloGroups := detectColoBatch(ctx, candidates, cfg.Port, cfg.ScanConcurrent, func(done, total int) {
			fmt.Printf("\r  Colo detection: %d/%d", done, total)
		})
		fmt.Println()

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

			fmt.Println("  Colo average latencies (lowest to highest):")
			for idx, s := range stats {
				marker := "  "
				if idx < 3 {
					marker = "★ "
				}
				fmt.Printf("  %s%-6s  %4d nodes  avg %.1fms\n", marker, s.name, s.count, s.avgMs)
			}

			var multiColoCandidates []NodeResult
			numColos := 3
			if len(stats) < numColos {
				numColos = len(stats)
			}
			for i := 0; i < numColos; i++ {
				multiColoCandidates = append(multiColoCandidates, coloGroups[stats[i].name]...)
			}
			candidates = multiColoCandidates
			fmt.Printf("  → %d candidates selected from top %d Colos\n", len(candidates), numColos)
		} else {
			fmt.Println("  [!] No valid Colo detected, testing all candidates")
		}

	default: // "none" or fallback
		if len(candidates) > cfg.TopN {
			candidates = candidates[:cfg.TopN]
		}
		fmt.Printf("\n🚀 Skipping candidate filtering. Testing top %d candidates directly.\n", len(candidates))
	}

	if len(candidates) == 0 {
		fmt.Println("[!] No candidates selected for testing.")
		return
	}

	fmt.Printf("\n🚀 Download Test (%ds duration, %d parallel)\n", cfg.Duration, cfg.DLConc)
	if cfg.SkipLoadLatency {
		fmt.Printf("%-16s %-6s %-9s %-9s %-13s %-12s %-8s %-6s\n",
			"IP", "Colo", "Latency", "Jitter", "Speed", "MinSpd", "Stable", "Score")
		fmt.Println("--------------------------------------------------------------------------")
	} else {
		fmt.Printf("%-16s %-6s %-9s %-9s %-13s %-12s %-9s %-8s %-6s\n",
			"IP", "Colo", "Latency", "Jitter", "Speed", "MinSpd", "LoadLat", "Stable", "Score")
		fmt.Println("-------------------------------------------------------------------------------------------")
	}

	results := runParallelDownloadTest(ctx, candidates, cfg, func(res NodeResult) {
		if res.Colo != "429" || !cfg.Skip429 {
			fmt.Printf("\r%-130s\r", "")
			if cfg.SkipLoadLatency {
				fmt.Printf("%-16s %-6s %6.1fms  %5.1fms  %6.2f MB/s  %5.2f MB/s  %4.0f%%   %5.1f\n",
					res.IP, res.Colo, res.TCPLatency, res.Jitter,
					res.DownloadSpeed, res.MinSpeed, res.Stability, res.Score)
			} else {
				fmt.Printf("%-16s %-6s %6.1fms  %5.1fms  %6.2f MB/s  %5.2f MB/s  %6.1fms  %4.0f%%   %5.1f\n",
					res.IP, res.Colo, res.TCPLatency, res.Jitter,
					res.DownloadSpeed, res.MinSpeed, res.LoadLatency, res.Stability, res.Score)
			}
		}
	}, nil, func(p LiveProgress) {
		fmt.Printf("\r  📥 %-16s %6.1f MB  %6.2f MB/s  %4.0f/%ds    ",
			p.IP, float64(p.Bytes)/1024/1024, p.Speed, p.Elapsed, int(p.Duration))
	}, func() {
		fmt.Println("\n⚡ Fast-exit triggered.")
	})

	if len(results) == 0 {
		fmt.Println("\n[!] All tested IPs failed or were rate-limited.")
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

	f.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	w := csv.NewWriter(f)
	defer w.Flush()

	w.Write([]string{"IP", "Colo", "Latency", "Jitter", "SgSpeed_MB", "Speed_MB", "MinSpeed_MB", "LoadLatency", "Stability", "Score"})
	for _, r := range results {
		w.Write([]string{
			r.IP, r.Colo,
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
