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
	SNI             string
	WSSHost         string
	WSSPath         string
	Profile         string // "CFST" (default), "GOWAY-WSS", "CUSTOM"

	// Route Quality Probe daemon options
	DaemonMode         bool
	APIAddr            string
	ActiveInterval     int    // seconds
	StandbyInterval    int    // seconds
	CandidateInterval  int    // seconds
	FailedInterval     int    // seconds
	ScoreMode          string // "normal" or "peak"
	StateFile          string // path to state snapshot json
	DiscoveryEnabled   bool   // periodic continuous discovery in background
	DiscoveryInterval  int    // seconds between discovery passes (default 3600)
	DiscoveryScanCount int    // random IPs scanned per pass (default 200)
}

func DefaultConfig() Config {
	return Config{
		Port:               443,
		MaxScan:            3000,
		TopN:               100,
		DLConc:             1,
		DownloadNum:        20,
		Duration:           20,
		StopThreshold:      30.0,
		Unique:             false,
		Output:             "result_colo.csv",
		ScanConcurrent:     200,
		WebPort:            "9876",
		URL:                "https://speed.cloudflare.com/__down?bytes=500000000",
		Skip429:            true,
		QuickDuration:      3,
		FilterMode:         "speed",
		WSSHost:            "",
		WSSPath:            "/pyway",
		Profile:            "CFST",
		DaemonMode:         false,
		APIAddr:            "127.0.0.1:9876",
		ActiveInterval:     10,
		StandbyInterval:    30,
		CandidateInterval:  180,
		FailedInterval:     60,
		ScoreMode:          "normal",
		StateFile:          "cfst_state.json",
		DiscoveryEnabled:   true,
		DiscoveryInterval:  3600,
		DiscoveryScanCount: 200,
	}
}

// GetProbeProfile returns the effective ProbeProfile derived strictly from Config.
// The default is strictly ProfileCFST unless cfg.Profile is explicitly "GOWAY-WSS" or "CUSTOM".
func (cfg Config) GetProbeProfile() ProbeProfile {
	switch strings.ToUpper(strings.TrimSpace(cfg.Profile)) {
	case "GOWAY-WSS", "GOWAY_WSS", "WSS":
		path := cfg.WSSPath
		if path == "" {
			path = "/pyway"
		}
		return NewProfileGOWAYWSS(cfg.WSSHost, path, cfg.SNI, cfg.Port)
	case "CUSTOM":
		return NewProfileCustom(cfg.URL, cfg.SNI, cfg.Port)
	default:
		if isCustomURL(cfg.URL) {
			return NewProfileCustom(cfg.URL, cfg.SNI, cfg.Port)
		}
		return NewProfileCFST()
	}
}

func isCustomURL(urlStr string) bool {
	return !strings.Contains(urlStr, "speed.cloudflare.com/__down")
}

// ScanRoutesWithProfile runs profile-driven candidate discovery.
// - ProfileCFST: TCP Ping + HTTPSConnectivityCheck (strictly NO WSS, NEVER calls WSSHandshakeCheck).
// - ProfileGOWAYWSS: TCP Ping + WSSHandshakeCheck (using Profile.Host, Profile.SNI, Profile.Path).
// - ProfileCustom: TCP Ping + (HTTPSConnectivityCheck if http/https, WSSHandshakeCheck if wss).
func ScanRoutesWithProfile(ctx context.Context, ips []string, port int, concurrency int, profile ProbeProfile, progressCallback func(done, total, valid int)) []NodeResult {
	nodes, _, _ := ScanRoutesWithProfileDetailed(ctx, ips, port, concurrency, profile, progressCallback)
	return nodes
}

// ScanRoutesWithProfileDetailed runs profile-driven candidate discovery and returns valid nodes along with TCP valid and L2 valid counts.
func ScanRoutesWithProfileDetailed(ctx context.Context, ips []string, port int, concurrency int, profile ProbeProfile, progressCallback func(done, total, valid int)) ([]NodeResult, int, int) {
	if profile.Type == "" {
		profile = NewProfileCFST()
	}
	targetPort := port
	if targetPort <= 0 {
		targetPort = profile.Port
	}
	if targetPort <= 0 {
		targetPort = 443
	}

	var validNodes []NodeResult
	var mu sync.Mutex
	var done, validCount, tcpValidCount, l2ValidCount atomic.Int32
	total := len(ips)

	if concurrency < 1 {
		concurrency = 1
	}
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

			// Layer 1: TCP Ping (5 pings)
			pingCount := 5
			lats := make([]float64, 0, 5)
			for i := 0; i < pingCount; i++ {
				if ctx.Err() != nil {
					return
				}
				lat := TCPPing(ip, targetPort, 1500*time.Millisecond)
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
			if len(lats) < 3 { // require at least 3 successful pings (up to 40% loss tolerated)
				if progressCallback != nil && (d%10 == 0 || d == int32(total)) {
					progressCallback(int(d), total, int(validCount.Load()))
				}
				return
			}

			tcpValidCount.Add(1)

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

			// Layer 2: Profile-driven check
			var detectedColo string
			var wssRes WSSHandshakeResult
			switch profile.Type {
			case ProfileCFST:
				// TCP Ping + HTTPSConnectivityCheck (Cloudflare Official, strictly NO WSS)
				ok, _, _, colo, err := HTTPSConnectivityCheck(ctx, ip, targetPort, profile.SNI, profile.Host, profile.TestURL, 3*time.Second)
				if !ok || err != nil {
					if progressCallback != nil && (d%10 == 0 || d == int32(total)) {
						progressCallback(int(d), total, int(validCount.Load()))
					}
					return
				}
				detectedColo = colo

			case ProfileGOWAYWSS:
				// TCP Ping + WSSHandshakeCheckDetailed using Profile fields
				wssRes = WSSHandshakeCheckDetailed(ip, targetPort, profile.SNI, profile.Host, profile.Path, 3*time.Second)
				if !wssRes.Success {
					if progressCallback != nil && (d%10 == 0 || d == int32(total)) {
						progressCallback(int(d), total, int(validCount.Load()))
					}
					return
				}

			case ProfileCustom:
				if profile.Protocol == "wss" {
					wssRes = WSSHandshakeCheckDetailed(ip, targetPort, profile.SNI, profile.Host, profile.Path, 3*time.Second)
					if !wssRes.Success {
						if progressCallback != nil && (d%10 == 0 || d == int32(total)) {
							progressCallback(int(d), total, int(validCount.Load()))
						}
						return
					}
				} else {
					ok, _, _, colo, err := HTTPSConnectivityCheck(ctx, ip, targetPort, profile.SNI, profile.Host, profile.TestURL, 3*time.Second)
					if !ok || err != nil {
						if progressCallback != nil && (d%10 == 0 || d == int32(total)) {
							progressCallback(int(d), total, int(validCount.Load()))
						}
						return
					}
					detectedColo = colo
				}
			}

			l2ValidCount.Add(1)

			node := NodeResult{
				IP:         ip,
				Port:       targetPort,
				TCPLatency: avgLat,
				Jitter:     jitter,
				PacketLoss: loss,
				Colo:       detectedColo,
			}
			if profile.Type == ProfileGOWAYWSS || (profile.Type == ProfileCustom && profile.Protocol == "wss") {
				node.GOWAYWSSCompatible = wssRes.Success
				node.GOWAYWSSLatency = wssRes.Latency
				node.GOWAYWSSErrorStage = wssRes.ErrorStage
				node.GOWAYWSSHTTPStatus = wssRes.HTTPStatus
				node.GOWAYWSSErrorMessage = wssRes.ErrorMessage
				node.GOWAYWSSSNISent = wssRes.SNISent
				node.GOWAYWSSHostSent = wssRes.HostSent
				node.GOWAYWSSPathSent = wssRes.PathSent
			}

			mu.Lock()
			validNodes = append(validNodes, node)
			mu.Unlock()
			validCount.Add(1)

			if progressCallback != nil && (d%10 == 0 || d == int32(total)) {
				progressCallback(int(d), total, int(validCount.Load()))
			}
		}(ip)
	}
	wg.Wait()
	return validNodes, int(tcpValidCount.Load()), int(l2ValidCount.Load())
}

// ScanPing runs 5 TCP pings per IP and filters by packet loss.
// Legacy compatibility wrapper for CLI; for profile-driven scanning, use ScanRoutesWithProfile.
func ScanPing(ctx context.Context, ips []string, port int, concurrency int, wssHost string, progressCallback func(done, total, valid int)) []NodeResult {
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

				if wssHost != "" && !WSSHandshakeCheck(ip, port, wssHost, wssHost, "/pyway", 3*time.Second) {
					if progressCallback != nil && (d%10 == 0 || d == int32(total)) {
						progressCallback(int(d), total, int(validCount.Load()))
					}
					return
				}

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
			target := ResolveProbeTarget(ProbeConfig{Profile: cfg.GetProbeProfile()}, ip, cfg.Port)
			sm := SingleStreamTestDetailed(ctx, target, cfg.QuickDuration, nil, 0, 0, 0)
			results[idx] = quickResult{idx: idx, speed: sm.AverageSpeed}
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

				candTarget := ResolveProbeTarget(ProbeConfig{Profile: cfg.GetProbeProfile()}, cand.IP, cfg.Port)
				sm := SingleStreamTestDetailed(ctx, candTarget, cfg.Duration, progressLive, cand.TCPLatency, cand.Jitter, cand.PacketLoss)

				if sm.AverageSpeed == 0 && sm.MinSpeed == 0 && sm.Stability == 0 {
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
					if candTarget.ProfileType == ProfileCFST && strings.Contains(candTarget.URL, "speed.cloudflare.com") {
						cand.Colo = GetColo(cand.IP, cfg.Port)
						if !cfg.SkipLoadLatency {
							cand.LoadLatency = MeasureLoadLatency(cand.IP, cfg.Port)
						}
					}
					cand.DownloadSpeed = sm.AverageSpeed
					cand.SingleSpeed = sm.AverageSpeed
					cand.P10Speed = sm.P10Speed
					cand.MedianSpeed = sm.MedianSpeed
					cand.MinSpeed = sm.MinSpeed
					cand.Stability = sm.Stability
					cand.StallCount = sm.StallCount
					cand.ZeroSpeedIntervals = sm.ZeroSpeedIntervals
					cand.CalcScore()

					select {
					case resultCh <- cand:
					case <-doneCh:
						return
					}

					if sm.AverageSpeed >= cfg.StopThreshold {
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
	fmt.Printf("Cloudflare SpeedTest v2.1.9-dev (Route Quality Probe - Go Edition)\n\n")

	ips := GenerateIPs(cfg.MaxScan, cfg.Unique, cfg.IPFile)
	fmt.Printf("🔍 Scanning %d IPs (concurrency: %d)...\n", len(ips), cfg.ScanConcurrent)

	ctx := context.Background()

	validNodes := ScanRoutesWithProfile(ctx, ips, cfg.Port, cfg.ScanConcurrent, cfg.GetProbeProfile(), func(done, total, valid int) {
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
		fmt.Printf("%-16s %-6s %-9s %-9s %-12s %-11s %-11s %-8s %-6s\n",
			"IP", "Colo", "Latency", "Jitter", "Speed", "P10", "MinSpd", "Stable", "Score")
		fmt.Println("------------------------------------------------------------------------------------------------")
	} else {
		fmt.Printf("%-16s %-6s %-9s %-9s %-12s %-11s %-11s %-9s %-8s %-6s\n",
			"IP", "Colo", "Latency", "Jitter", "Speed", "P10", "MinSpd", "LoadLat", "Stable", "Score")
		fmt.Println("-------------------------------------------------------------------------------------------------------------")
	}

	results := runParallelDownloadTest(ctx, candidates, cfg, func(res NodeResult) {
		if res.Colo != "429" || !cfg.Skip429 {
			fmt.Printf("\r%-140s\r", "")
			if cfg.SkipLoadLatency {
				fmt.Printf("%-16s %-6s %6.1fms  %5.1fms  %5.2f MB/s  %5.2f MB/s  %5.2f MB/s  %4.0f%%   %5.1f\n",
					res.IP, res.Colo, res.TCPLatency, res.Jitter,
					res.DownloadSpeed, res.P10Speed, res.MinSpeed, res.Stability, res.Score)
			} else {
				fmt.Printf("%-16s %-6s %6.1fms  %5.1fms  %5.2f MB/s  %5.2f MB/s  %5.2f MB/s  %6.1fms  %4.0f%%   %5.1f\n",
					res.IP, res.Colo, res.TCPLatency, res.Jitter,
					res.DownloadSpeed, res.P10Speed, res.MinSpeed, res.LoadLatency, res.Stability, res.Score)
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

	for i, res := range results {
		tier := TierCandidate
		if i == 0 {
			tier = TierActive
		} else if i <= 2 {
			tier = TierStandby
		}
		rm := FromNodeResult(res, tier)
		GlobalRouteStore.UpsertRoute(*rm)
	}

	saveCSV(cfg.Output, results)
	fmt.Printf("\n💾 Saved to: %s\n", cfg.Output)
	if cfg.StateFile != "" {
		_ = GlobalRouteStore.SaveSnapshot(cfg.StateFile)
		fmt.Printf("💾 Saved route state to: %s\n", cfg.StateFile)
	}

	if cfg.DaemonMode {
		fmt.Println("\n⚡ Transitioning to Route Quality Probe daemon...")
		RunDaemon(cfg)
	}
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

	w.Write([]string{"IP", "Colo", "Latency", "Jitter", "SgSpeed_MB", "Speed_MB", "P10Speed_MB", "MinSpeed_MB", "LoadLatency", "Stability", "Score"})
	for _, r := range results {
		w.Write([]string{
			r.IP, r.Colo,
			fmt.Sprintf("%.1f", r.TCPLatency),
			fmt.Sprintf("%.1f", r.Jitter),
			fmt.Sprintf("%.2f", r.SingleSpeed),
			fmt.Sprintf("%.2f", r.DownloadSpeed),
			fmt.Sprintf("%.2f", r.P10Speed),
			fmt.Sprintf("%.2f", r.MinSpeed),
			fmt.Sprintf("%.1f", r.LoadLatency),
			fmt.Sprintf("%.0f", r.Stability),
			fmt.Sprintf("%.1f", r.Score),
		})
	}
}
