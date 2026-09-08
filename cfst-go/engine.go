package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var CloudflareIPv4Ranges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	// High-performance Cloudflare subnets (DNS, Pages, Workers, WARP)
	"162.159.36.0/22", "162.159.44.0/24", "162.159.192.0/24", "162.159.193.0/24",
}

type cidrInfo struct {
	baseIP   uint32
	maxHost  int
	hostBits int
}

var cidrCache sync.Map

func parseCIDRCached(cidr string) *cidrInfo {
	if v, ok := cidrCache.Load(cidr); ok {
		return v.(*cidrInfo)
	}
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	ones, bits := ipNet.Mask.Size()
	hostBits := bits - ones
	if hostBits <= 2 {
		cidrCache.Store(cidr, &cidrInfo{baseIP: binary.BigEndian.Uint32(ipNet.IP.To4()), maxHost: 0, hostBits: hostBits})
	} else {
		cidrCache.Store(cidr, &cidrInfo{baseIP: binary.BigEndian.Uint32(ipNet.IP.To4()), maxHost: (1 << hostBits) - 2, hostBits: hostBits})
	}
	v, _ := cidrCache.Load(cidr)
	return v.(*cidrInfo)
}

type NodeResult struct {
	IP                 string  `json:"ip"`
	Port               int     `json:"port"`
	TCPLatency         float64 `json:"tcp_latency"`
	DownloadSpeed      float64 `json:"download_speed"`
	SingleSpeed        float64 `json:"single_speed"`
	LoadLatency        float64 `json:"load_latency"`
	Colo               string  `json:"colo"`
	Score              float64 `json:"score"`
	Jitter             float64 `json:"jitter"`
	Stability          float64 `json:"stability"`
	MinSpeed           float64 `json:"min_speed"`
	P10Speed           float64 `json:"p10_speed"`
	MedianSpeed        float64 `json:"median_speed"`
	StallCount         int     `json:"stall_count"`
	ZeroSpeedIntervals int     `json:"zero_speed_intervals"`
	PacketLoss         float64 `json:"packet_loss"`
	// GOWAY WSS compatibility gate (ProfileGOWAYWSS)
	GOWAYWSSCompatible   bool    `json:"goway_wss_compatible,omitempty"`
	GOWAYWSSLatency      float64 `json:"goway_wss_latency,omitempty"`
	GOWAYWSSErrorStage   string  `json:"goway_wss_error_stage,omitempty"`
	GOWAYWSSHTTPStatus   int     `json:"goway_wss_http_status,omitempty"`
	GOWAYWSSErrorMessage string  `json:"goway_wss_error_message,omitempty"`
	GOWAYWSSSNISent      string  `json:"goway_wss_sni_sent,omitempty"`
	GOWAYWSSHostSent     string  `json:"goway_wss_host_sent,omitempty"`
	GOWAYWSSPathSent     string  `json:"goway_wss_path_sent,omitempty"`
}

func (n *NodeResult) CalcScore() {
	w := GlobalScoreEngine.ActiveWeights()
	effectiveSpeed := n.DownloadSpeed
	if n.SingleSpeed > 0 {
		effectiveSpeed = n.SingleSpeed
	}
	effectiveP10 := n.P10Speed
	if effectiveP10 <= 0 && effectiveSpeed > 0 {
		effectiveP10 = n.MinSpeed
	}
	n.Score = GlobalScoreEngine.calcMetricScore(w, effectiveSpeed, effectiveP10, n.MinSpeed, n.TCPLatency, n.Jitter, n.PacketLoss, n.Stability, true, n.Colo)
}

func randIPFromCIDR(cidr string) string {
	info := parseCIDRCached(cidr)
	if info == nil {
		return ""
	}
	if info.hostBits <= 2 {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], info.baseIP)
		return net.IP(buf[:]).String()
	}
	offset := rand.Intn(info.maxHost) + 1
	ip := info.baseIP + uint32(offset)
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], ip)
	return net.IP(buf[:]).String()
}

// SpeedIntervalSample represents one discrete interval observation.
type SpeedIntervalSample struct {
	Timestamp       time.Time `json:"timestamp"`
	DeltaBytes      int64     `json:"delta_bytes"`
	DeltaDuration   float64   `json:"delta_duration"`
	IntervalSpeed   float64   `json:"interval_speed"`   // MB/s
	CumulativeSpeed float64   `json:"cumulative_speed"` // MB/s
	Elapsed         float64   `json:"elapsed"`          // seconds
	TotalBytes      int64     `json:"total_bytes"`
	IsStall         bool      `json:"is_stall"`
}

// SpeedMetrics contains comprehensive statistics derived from speed interval samples.
type SpeedMetrics struct {
	AverageSpeed           float64               `json:"avg_speed"`
	MedianSpeed            float64               `json:"median_speed"`
	P10Speed               float64               `json:"p10_speed"`
	P25Speed               float64               `json:"p25_speed"`
	MinSpeed               float64               `json:"min_speed"`
	MaxSpeed               float64               `json:"max_speed"`
	StdDev                 float64               `json:"std_dev"`
	CoefficientOfVariation float64               `json:"cv"`
	Stability              float64               `json:"stability"` // 0.0 - 100.0
	ZeroSpeedIntervals     int                   `json:"zero_speed_intervals"`
	StallCount             int                   `json:"stall_count"`
	TotalStallDuration     float64               `json:"total_stall_duration"`
	LongestStallDuration   float64               `json:"longest_stall_duration"`
	StallRate              float64               `json:"stall_rate"` // TotalStallDuration / TotalDuration
	Intervals              []SpeedIntervalSample `json:"intervals,omitempty"`
}

// calcPercentile computes the p-th percentile (p in [0.0, 1.0]) using linear interpolation on sorted data.
func calcPercentile(sortedVals []float64, p float64) float64 {
	if len(sortedVals) == 0 {
		return 0
	}
	if len(sortedVals) == 1 || p <= 0 {
		return sortedVals[0]
	}
	if p >= 1.0 {
		return sortedVals[len(sortedVals)-1]
	}
	idx := p * float64(len(sortedVals)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper {
		return sortedVals[lower]
	}
	weight := idx - float64(lower)
	return sortedVals[lower]*(1.0-weight) + sortedVals[upper]*weight
}

// CalcCompositeStability computes multi-dimensional stability (0 - 100):
// SpeedConsistency * 0.35 + FloorStability * 0.25 + NoStallRatio * 0.20 + LatencyConsistency * 0.10 + LossConsistency * 0.10
func CalcCompositeStability(sm *SpeedMetrics, tcpRTT, jitter, packetLoss float64) float64 {
	if sm == nil {
		return 0
	}

	// 1. SpeedConsistency: based on CV
	speedConsistency := 100.0 - sm.CoefficientOfVariation*100.0
	if speedConsistency < 0 {
		speedConsistency = 0
	} else if speedConsistency > 100.0 {
		speedConsistency = 100.0
	}

	// 2. FloorStability: P10 / AverageSpeed ratio (guarantees anti-buffering floor)
	floorStability := 0.0
	if sm.AverageSpeed > 0.001 {
		floorStability = (sm.P10Speed / sm.AverageSpeed) * 100.0
		if floorStability < 0 {
			floorStability = 0
		} else if floorStability > 100.0 {
			floorStability = 100.0
		}
	}

	// 3. NoStallRatio: ratio of non-stall duration
	noStallRatio := (1.0 - sm.StallRate) * 100.0
	if noStallRatio < 0 {
		noStallRatio = 0
	} else if noStallRatio > 100.0 {
		noStallRatio = 100.0
	}

	// 4. LatencyConsistency: jitter compared to RTT
	latencyConsistency := 100.0
	if tcpRTT > 0 {
		latencyConsistency = 100.0 - (jitter/(tcpRTT+1.0))*100.0
		if latencyConsistency < 0 {
			latencyConsistency = 0
		} else if latencyConsistency > 100.0 {
			latencyConsistency = 100.0
		}
	}

	// 5. LossConsistency: packet loss percentage
	lossConsistency := (1.0 - packetLoss) * 100.0
	if lossConsistency < 0 {
		lossConsistency = 0
	} else if lossConsistency > 100.0 {
		lossConsistency = 100.0
	}

	composite := speedConsistency*0.35 +
		floorStability*0.25 +
		noStallRatio*0.20 +
		latencyConsistency*0.10 +
		lossConsistency*0.10

	if math.IsNaN(composite) || math.IsInf(composite, 0) || composite < 0 {
		composite = 0
	} else if composite > 100.0 {
		composite = 100.0
	}
	return math.Round(composite*10) / 10
}

// ProcessIntervalSamples processes collected intervals into comprehensive SpeedMetrics.
// Crucially, zero speed intervals are NEVER discarded.
func ProcessIntervalSamples(intervals []SpeedIntervalSample, totalBytes int64, totalDuration float64, tcpRTT, jitter, packetLoss float64) SpeedMetrics {
	sm := SpeedMetrics{
		Intervals: intervals,
	}
	if totalDuration <= 0.01 {
		return sm
	}

	finalMB := float64(totalBytes) / 1024.0 / 1024.0
	sm.AverageSpeed = finalMB / totalDuration

	if len(intervals) == 0 {
		sm.MinSpeed = 0
		sm.MedianSpeed = sm.AverageSpeed
		sm.P10Speed = sm.AverageSpeed
		sm.P25Speed = sm.AverageSpeed
		sm.MaxSpeed = sm.AverageSpeed
		sm.Stability = CalcCompositeStability(&sm, tcpRTT, jitter, packetLoss)
		return sm
	}

	speeds := make([]float64, len(intervals))
	var sum float64
	minSpd := math.MaxFloat64
	maxSpd := 0.0

	curConsecutiveStalls := 0
	curConsecutiveStallDur := 0.0
	for i, it := range intervals {
		s := it.IntervalSpeed
		speeds[i] = s
		sum += s
		if s < minSpd {
			minSpd = s
		}
		if s > maxSpd {
			maxSpd = s
		}

		if it.IsStall {
			sm.ZeroSpeedIntervals++
			sm.TotalStallDuration += it.DeltaDuration
			curConsecutiveStalls++
			curConsecutiveStallDur += it.DeltaDuration
			if curConsecutiveStallDur > sm.LongestStallDuration {
				sm.LongestStallDuration = curConsecutiveStallDur
			}
		} else {
			if curConsecutiveStalls > 0 {
				sm.StallCount++
				curConsecutiveStalls = 0
				curConsecutiveStallDur = 0.0
			}
		}
	}
	if curConsecutiveStalls > 0 {
		sm.StallCount++
	}

	if totalDuration > 0 {
		sm.StallRate = sm.TotalStallDuration / totalDuration
		if sm.StallRate > 1.0 {
			sm.StallRate = 1.0
		}
	}

	if minSpd == math.MaxFloat64 {
		minSpd = 0
	}
	sm.MinSpeed = minSpd
	sm.MaxSpeed = maxSpd

	// Mean and CV over all intervals (including 0s)
	mean := sum / float64(len(speeds))
	if len(speeds) > 1 && mean > 0.001 {
		var variance float64
		for _, s := range speeds {
			diff := s - mean
			variance += diff * diff
		}
		variance /= float64(len(speeds))
		sm.StdDev = math.Sqrt(variance)
		sm.CoefficientOfVariation = sm.StdDev / mean
	} else if mean <= 0.001 {
		sm.CoefficientOfVariation = 1.0
	}

	// Percentiles on sorted copy
	sortedSpeeds := make([]float64, len(speeds))
	copy(sortedSpeeds, speeds)
	sort.Float64s(sortedSpeeds)

	sm.P10Speed = calcPercentile(sortedSpeeds, 0.10)
	sm.P25Speed = calcPercentile(sortedSpeeds, 0.25)
	sm.MedianSpeed = calcPercentile(sortedSpeeds, 0.50)

	sm.Stability = CalcCompositeStability(&sm, tcpRTT, jitter, packetLoss)
	return sm
}

// SingleStreamTestDetailed runs true 1s interval sampling and returns full SpeedMetrics.
// It executes strictly driven by the provided ResolvedProbeTarget (Target.URL, Target.SNI, Target.Host, Target.Port, Target.Protocol).
func SingleStreamTestDetailed(ctx context.Context, target ResolvedProbeTarget, duration int,
	progressCallback func(LiveProgress), tcpRTT, jitter, packetLoss float64) SpeedMetrics {

	parsedURL, err := url.Parse(target.URL)
	if err != nil {
		return SpeedMetrics{}
	}

	client := makeHTTPClient(target.IP, target.Port, target.SNI)
	if tr, ok := client.Transport.(*http.Transport); ok {
		defer tr.CloseIdleConnections()
	}

	dur := time.Duration(duration) * time.Second
	downloadCtx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	req, err := newCFRequestWithContext(downloadCtx, "GET", target.URL)
	if err != nil {
		return SpeedMetrics{}
	}
	req.Host = target.Host
	req.Header.Set("Connection", "keep-alive")

	if !strings.Contains(target.URL, "speed.cloudflare.com") {
		scheme := target.Protocol
		if scheme == "" {
			scheme = parsedURL.Scheme
		}
		if scheme == "" {
			scheme = "https"
		}
		baseURL := scheme + "://" + target.Host
		setCFHeadersForURL(req, baseURL)
	}

	resp, err := client.Do(req)
	if err != nil {
		return SpeedMetrics{}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return SpeedMetrics{}
	}

	startGlobal := time.Now()
	var totalBytes int64
	sampleInterval := 1 * time.Second
	var intervals []SpeedIntervalSample
	var sampleMu sync.Mutex

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		prevBytes := int64(0)
		prevTime := startGlobal

		for {
			select {
			case now := <-ticker.C:
				curBytes := atomic.LoadInt64(&totalBytes)
				deltaBytes := curBytes - prevBytes
				dt := now.Sub(prevTime).Seconds()
				if dt > 0.05 {
					spd := (float64(deltaBytes) / 1024.0 / 1024.0) / dt
					cumSpd := (float64(curBytes) / 1024.0 / 1024.0) / now.Sub(startGlobal).Seconds()
					isStall := (spd <= 0.01)

					sampleMu.Lock()
					intervals = append(intervals, SpeedIntervalSample{
						Timestamp:       now,
						DeltaBytes:      deltaBytes,
						DeltaDuration:   dt,
						IntervalSpeed:   spd,
						CumulativeSpeed: cumSpd,
						Elapsed:         now.Sub(startGlobal).Seconds(),
						TotalBytes:      curBytes,
						IsStall:         isStall,
					})
					sampleMu.Unlock()

					prevBytes = curBytes
					prevTime = now

					if progressCallback != nil {
						progressCallback(LiveProgress{
							IP:       target.IP,
							Bytes:    curBytes,
							Speed:    cumSpd,
							Elapsed:  now.Sub(startGlobal).Seconds(),
							Duration: float64(duration),
						})
					}
				}
			case <-downloadCtx.Done():
				return
			case <-done:
				return
			}
		}
	}()

	bufPtr := downloadBufPool.Get().(*[]byte)
	buf := *bufPtr
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			atomic.AddInt64(&totalBytes, int64(n))
		}
		if err != nil {
			break
		}
	}
	downloadBufPool.Put(bufPtr)
	close(done)

	realTime := time.Since(startGlobal).Seconds()
	if realTime < 0.1 {
		return SpeedMetrics{}
	}

	// Final interval drain if not recorded
	finalBytes := atomic.LoadInt64(&totalBytes)
	sampleMu.Lock()
	if len(intervals) > 0 {
		lastIt := intervals[len(intervals)-1]
		remainDt := realTime - lastIt.Elapsed
		if remainDt >= 0.2 {
			remainBytes := finalBytes - lastIt.TotalBytes
			if remainBytes < 0 {
				remainBytes = 0
			}
			spd := (float64(remainBytes) / 1024.0 / 1024.0) / remainDt
			intervals = append(intervals, SpeedIntervalSample{
				Timestamp:       time.Now(),
				DeltaBytes:      remainBytes,
				DeltaDuration:   remainDt,
				IntervalSpeed:   spd,
				CumulativeSpeed: (float64(finalBytes) / 1024.0 / 1024.0) / realTime,
				Elapsed:         realTime,
				TotalBytes:      finalBytes,
				IsStall:         spd <= 0.01,
			})
		}
	} else if realTime > 0.05 {
		spd := (float64(finalBytes) / 1024.0 / 1024.0) / realTime
		intervals = append(intervals, SpeedIntervalSample{
			Timestamp:       time.Now(),
			DeltaBytes:      finalBytes,
			DeltaDuration:   realTime,
			IntervalSpeed:   spd,
			CumulativeSpeed: spd,
			Elapsed:         realTime,
			TotalBytes:      finalBytes,
			IsStall:         spd <= 0.01,
		})
	}
	intervalsCopy := make([]SpeedIntervalSample, len(intervals))
	copy(intervalsCopy, intervals)
	sampleMu.Unlock()

	return ProcessIntervalSamples(intervalsCopy, finalBytes, realTime, tcpRTT, jitter, packetLoss)
}

// SingleStreamTest measures single-connection download speed (backward-compatible wrapper).
// Returns avgSpeed (MB/s), minSpeed (MB/s), stability (0-100).
func SingleStreamTest(ctx context.Context, ip string, port int, duration int, testURL string, customSNI string,
	progressCallback func(LiveProgress)) (avgSpeed, minSpeed, stability float64) {
	target := ResolveProbeTarget(ProbeConfig{Profile: NewProfileCustom(testURL, customSNI, port)}, ip, port)
	sm := SingleStreamTestDetailed(ctx, target, duration, progressCallback, 0, 0, 0)
	return sm.AverageSpeed, sm.MinSpeed, sm.Stability
}

// MeasureLoadLatency measures TCP latency while a download is saturating the connection.
// Uses speed.cloudflare.com as the load source (only relevant for CF URL mode).
func MeasureLoadLatency(ip string, port int) float64 {
	testURL := "https://speed.cloudflare.com/__down?bytes=10000000"
	parsedURL, _ := url.Parse(testURL)
	host := parsedURL.Hostname()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	tr := &http.Transport{
		TLSClientConfig:     sharedTLSConfig,
		MaxIdleConnsPerHost: 10,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			addr := net.JoinHostPort(ip, strconv.Itoa(port))
			return net.DialTimeout("tcp", addr, 2*time.Second)
		},
	}
	client := &http.Client{Transport: tr}
	defer tr.CloseIdleConnections()

	req, err := newCFRequestWithContext(ctx, "GET", testURL)
	if err != nil {
		return 0
	}
	req.Host = host

	go func() {
		resp, err := client.Do(req)
		if err == nil {
			bufPtr := downloadBufPool.Get().(*[]byte)
			buf := *bufPtr
			for {
				if _, err := resp.Body.Read(buf); err != nil {
					break
				}
			}
			resp.Body.Close()
			downloadBufPool.Put(bufPtr)
		}
	}()

	time.Sleep(500 * time.Millisecond)

	var lats []float64
	for i := 0; i < 3; i++ {
		lat := TCPPing(ip, port, 2*time.Second)
		if lat > 0 {
			lats = append(lats, lat)
		}
		if i < 2 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	if len(lats) == 0 {
		return 0
	}
	var s float64
	for _, l := range lats {
		s += l
	}
	return s / float64(len(lats))
}

func getRangeHostCount(r string) int64 {
	if !strings.Contains(r, "/") {
		return 1
	}
	_, ipNet, err := net.ParseCIDR(r)
	if err != nil {
		return 1
	}
	ones, bits := ipNet.Mask.Size()
	hostBits := bits - ones
	if hostBits < 0 || hostBits > 32 {
		return 1
	}
	return int64(1) << uint(hostBits)
}

func GenerateIPs(maxScan int, unique bool, ipFile string) []string {
	if maxScan <= 0 {
		return nil
	}
	ranges := CloudflareIPv4Ranges
	if ipFile != "" {
		if content, err := os.ReadFile(ipFile); err == nil {
			lines := strings.Split(string(content), "\n")
			var fileRanges []string
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					fileRanges = append(fileRanges, line)
				}
			}
			if len(fileRanges) > 0 {
				ranges = fileRanges
			}
		}
	}

	var totalHosts int64
	rangeHosts := make([]int64, len(ranges))
	for i, r := range ranges {
		h := getRangeHostCount(r)
		rangeHosts[i] = h
		totalHosts += h
	}

	var ips []string
	if unique {
		seen := make(map[string]bool)
		attempts := 0
		maxAttempts := maxScan * 5
		for len(ips) < maxScan && attempts < maxAttempts {
			attempts++
			var r string
			if totalHosts <= 0 {
				r = ranges[rand.Intn(len(ranges))]
			} else {
				val := int64(rand.Float64() * float64(totalHosts))
				var runningSum int64
				for idx, h := range rangeHosts {
					runningSum += h
					if val < runningSum {
						r = ranges[idx]
						break
					}
				}
				if r == "" {
					r = ranges[len(ranges)-1]
				}
			}

			if !strings.Contains(r, "/") {
				if !seen[r] {
					seen[r] = true
					ips = append(ips, r)
				}
				continue
			}
			ip := randIPFromCIDR(r)
			if ip == "" {
				continue
			}
			parts := strings.Split(ip, ".")
			if len(parts) == 4 {
				subnet := parts[0] + "." + parts[1] + "." + parts[2]
				if !seen[subnet] {
					seen[subnet] = true
					ips = append(ips, ip)
				}
			}
		}
		return ips
	}

	for i, r := range ranges {
		hosts := rangeHosts[i]
		count := int(float64(hosts) / float64(totalHosts) * float64(maxScan))
		if count < 1 {
			count = 1
		}
		if !strings.Contains(r, "/") {
			ips = append(ips, r)
			continue
		}
		for j := 0; j < count; j++ {
			ip := randIPFromCIDR(r)
			if ip != "" {
				ips = append(ips, ip)
			}
		}
	}
	rand.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })
	if len(ips) > maxScan {
		ips = ips[:maxScan]
	}
	return ips
}

func TCPPing(ip string, port int, timeout time.Duration) float64 {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprintf("%d", port)), timeout)
	if err != nil {
		return 0
	}
	conn.Close()
	lat := float64(time.Since(start).Microseconds()) / 1000.0
	if lat <= 0 {
		return 0.001
	}
	return lat
}

// WSSHandshakeResult records the outcome and diagnostics of a GOWAY WSS compatibility check.
type WSSHandshakeResult struct {
	Success         bool    `json:"success"`
	TCPSuccess      bool    `json:"tcp_success"`
	TLSHandshake    bool    `json:"tls_handshake"`
	HTTPStatus      int     `json:"http_status"`
	Latency         float64 `json:"latency"`
	ErrorStage      string  `json:"error_stage,omitempty"`
	ErrorMessage    string  `json:"error_message,omitempty"`
	SNISent         string  `json:"sni_sent,omitempty"`
	HostSent        string  `json:"host_sent,omitempty"`
	PathSent        string  `json:"path_sent,omitempty"`
}

// WSSHandshakeCheckDetailed validates GOWAY WSS compatibility stage by stage:
// Config check -> TCP connect -> TLS handshake with SNI -> HTTP Upgrade with Host/Path -> HTTP 101 Switching Protocols.
// Parameters are strictly independent with no mutual fallbacks.
func WSSHandshakeCheckDetailed(ip string, port int, sni string, host string, path string, timeout time.Duration) WSSHandshakeResult {
	res := WSSHandshakeResult{
		SNISent:  sni,
		HostSent: host,
		PathSent: path,
	}

	// 1. Config Validation Stage:
	// IP, Port, SNI, Host, Path must all be present and valid.
	// Host and SNI must NOT fall back to each other.
	if ip == "" || port <= 0 || port > 65535 || strings.TrimSpace(sni) == "" || strings.TrimSpace(host) == "" || strings.TrimSpace(path) == "" {
		res.ErrorStage = "config"
		res.ErrorMessage = "missing required WSS parameter"
		return res
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	res.PathSent = path

	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	start := time.Now()

	// 2. TCP Stage
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		res.ErrorStage = "tcp"
		res.ErrorMessage = err.Error()
		return res
	}
	defer conn.Close()
	res.TCPSuccess = true

	// 3. TLS Stage
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         sni,
	}
	tlsConn := tls.Client(conn, tlsConf)
	if err := tlsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		res.ErrorStage = "tls"
		res.ErrorMessage = err.Error()
		return res
	}
	if err := tlsConn.Handshake(); err != nil {
		res.ErrorStage = "tls"
		res.ErrorMessage = err.Error()
		return res
	}
	res.TLSHandshake = true

	// 4. HTTP Request Stage
	req := fmt.Sprintf("GET %s HTTP/1.1\r\n", path) +
		fmt.Sprintf("Host: %s\r\n", host) +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36\r\n" +
		"\r\n"

	if _, err := tlsConn.Write([]byte(req)); err != nil {
		res.ErrorStage = "http"
		res.ErrorMessage = fmt.Sprintf("failed to send HTTP upgrade request: %v", err)
		return res
	}

	reader := bufio.NewReader(tlsConn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		res.ErrorStage = "http"
		res.ErrorMessage = fmt.Sprintf("failed to read HTTP status line: %v", err)
		return res
	}

	res.Latency = float64(time.Since(start).Microseconds()) / 1000.0
	if res.Latency <= 0 {
		res.Latency = 0.001
	}

	// 5. HTTP Status Validation Stage
	trimmed := strings.TrimRight(statusLine, "\r\n")
	parts := strings.SplitN(trimmed, " ", 3)
	if len(parts) < 2 {
		res.ErrorStage = "status"
		res.ErrorMessage = fmt.Sprintf("malformed HTTP status line: %q", trimmed)
		return res
	}

	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		res.ErrorStage = "status"
		res.ErrorMessage = fmt.Sprintf("invalid HTTP status code %q in status line: %q", parts[1], trimmed)
		return res
	}
	res.HTTPStatus = statusCode

	if statusCode == 101 {
		res.Success = true
		return res
	}

	res.ErrorStage = "status"
	res.ErrorMessage = fmt.Sprintf("expected HTTP 101, got status %d (%s)", statusCode, trimmed)
	return res
}

// WSSHandshakeCheck simulates goway's WebSocket upgrade handshake over TLS using target parameters.
// Returns true if the server responds with 101 Switching Protocols.
func WSSHandshakeCheck(ip string, port int, sni string, host string, path string, timeout time.Duration) bool {
	return WSSHandshakeCheckDetailed(ip, port, sni, host, path, timeout).Success
}

// HTTPSConnectivityCheck performs low-bandwidth L2 HTTPS/TLS reachability verification,
// measuring TTFB, validating HTTP status code (< 500), and extracting CDN Colo.
func HTTPSConnectivityCheck(ctx context.Context, ip string, port int, sni string, host string, testURL string, timeout time.Duration) (bool, float64, int, string, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	parsedURL, err := url.Parse(testURL)
	if err != nil {
		return false, 0, 0, "", fmt.Errorf("invalid test URL: %w", err)
	}
	if host == "" {
		host = parsedURL.Hostname()
	}
	if sni == "" {
		sni = host
	}

	client := makeHTTPClient(ip, port, sni)
	if tr, ok := client.Transport.(*http.Transport); ok {
		defer tr.CloseIdleConnections()
	}

	req, err := http.NewRequestWithContext(reqCtx, "HEAD", testURL, nil)
	if err != nil {
		return false, 0, 0, "", err
	}
	req.Host = host
	req.Header.Set("Connection", "close")

	if strings.Contains(testURL, "speed.cloudflare.com") {
		setCFHeaders(req)
	} else {
		scheme := parsedURL.Scheme
		if scheme == "" {
			scheme = "https"
		}
		baseURL := scheme + "://" + host
		if parsedURL.Port() != "" {
			baseURL += ":" + parsedURL.Port()
		}
		setCFHeadersForURL(req, baseURL)
	}

	t0 := time.Now()
	resp, err := client.Do(req)
	// If server rejects HEAD (e.g. 405 Method Not Allowed), retry with lightweight GET Range
	if err == nil && resp.StatusCode == http.StatusMethodNotAllowed {
		resp.Body.Close()
		reqGet, gErr := http.NewRequestWithContext(reqCtx, "GET", testURL, nil)
		if gErr == nil {
			reqGet.Host = host
			reqGet.Header.Set("Connection", "close")
			reqGet.Header.Set("Range", "bytes=0-1023")
			if strings.Contains(testURL, "speed.cloudflare.com") {
				setCFHeaders(reqGet)
			} else {
				baseURL := parsedURL.Scheme + "://" + host
				setCFHeadersForURL(reqGet, baseURL)
			}
			t0 = time.Now()
			resp, err = client.Do(reqGet)
		}
	}

	if err != nil {
		return false, 0, 0, "", err
	}
	defer resp.Body.Close()

	ttfb := float64(time.Since(t0).Microseconds()) / 1000.0
	statusCode := resp.StatusCode
	colo := ""

	cfRay := resp.Header.Get("cf-ray")
	if cfRay != "" {
		parts := strings.Split(cfRay, "-")
		if len(parts) >= 2 {
			colo = strings.ToUpper(parts[len(parts)-1])
		}
	}

	if statusCode >= 500 {
		return false, ttfb, statusCode, colo, fmt.Errorf("HTTP %d", statusCode)
	}

	return true, ttfb, statusCode, colo, nil
}

var coloRe = regexp.MustCompile(`colo=([A-Z]+)`)

var sharedTLSConfig = &tls.Config{InsecureSkipVerify: true}

func makeTLSConfig(sni string) *tls.Config {
	if sni == "" {
		return sharedTLSConfig
	}
	return &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         sni,
	}
}

var downloadBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 262144) // 256KB
		return &b
	},
}

func init() {
	for _, cidr := range CloudflareIPv4Ranges {
		parseCIDRCached(cidr)
	}
}

// makeHTTPClient creates an HTTP client that force-dials to the specified CF IP.
func makeHTTPClient(ip string, port int, sni string) *http.Client {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	tr := &http.Transport{
		TLSClientConfig:     makeTLSConfig(sni),
		MaxIdleConnsPerHost: 4,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 3*time.Second)
		},
	}
	return &http.Client{Transport: tr}
}

func setCFHeaders(req *http.Request) {
	setCFHeadersForURL(req, "https://speed.cloudflare.com")
}

func setCFHeadersForURL(req *http.Request, baseURL string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", baseURL+"/")
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Sec-Ch-Ua", `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
}

func newCFRequest(method, urlStr string) (*http.Request, error) {
	req, err := http.NewRequest(method, urlStr, nil)
	if err != nil {
		return nil, err
	}
	setCFHeaders(req)
	return req, nil
}

func newCFRequestWithContext(ctx context.Context, method, urlStr string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, nil)
	if err != nil {
		return nil, err
	}
	setCFHeaders(req)
	return req, nil
}

func GetColo(ip string, port int) string {
	client := makeHTTPClient(ip, port, "")
	if tr, ok := client.Transport.(*http.Transport); ok {
		defer tr.CloseIdleConnections()
	}
	client.Timeout = 4 * time.Second

	req, err := newCFRequest("GET", "https://speed.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		return "ERR"
	}

	resp, err := client.Do(req)
	if err != nil {
		return "ERR"
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "UNK"
	}
	if match := coloRe.FindSubmatch(body); match != nil {
		return string(match[1])
	}
	return "UNK"
}

// LiveProgress holds real-time download progress for a single IP.
type LiveProgress struct {
	IP       string  `json:"ip"`
	Bytes    int64   `json:"bytes"`
	Speed    float64 `json:"speed"` // MB/s
	Elapsed  float64 `json:"elapsed"`
	Duration float64 `json:"duration"`
}

// LightweightHTTPProbeResult contains the outcome of an L3 low-bandwidth HTTP/HTTPS probe.
type LightweightHTTPProbeResult struct {
	Success    bool    `json:"success"`
	StatusCode int     `json:"status_code"`
	TTFB       float64 `json:"ttfb_ms"`
	Speed      float64 `json:"speed_mb"`
	BytesRead  int64   `json:"bytes_read"`
	Colo       string  `json:"colo"`
	Error      string  `json:"error,omitempty"`
}

// LightweightHTTPProbeTarget performs a minimal-overhead HTTP check driven strictly by ResolvedProbeTarget.
func LightweightHTTPProbeTarget(ctx context.Context, target ResolvedProbeTarget) LightweightHTTPProbeResult {
	res := LightweightHTTPProbeResult{}
	client := makeHTTPClient(target.IP, target.Port, target.SNI)
	if tr, ok := client.Transport.(*http.Transport); ok {
		defer tr.CloseIdleConnections()
	}

	// For Cloudflare official speed test, request a small 100KB chunk instead of 500MB
	probeURL := target.URL
	if strings.Contains(target.URL, "speed.cloudflare.com/__down") {
		probeURL = "https://speed.cloudflare.com/__down?bytes=100000"
	}

	reqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", probeURL, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Host = target.Host
	req.Header.Set("Connection", "close")

	if strings.Contains(target.URL, "speed.cloudflare.com") {
		setCFHeaders(req)
	} else {
		parsedURL, err := url.Parse(target.URL)
		scheme := target.Protocol
		if scheme == "" && err == nil && parsedURL.Scheme != "" {
			scheme = parsedURL.Scheme
		}
		if scheme == "" {
			scheme = "https"
		}
		baseURL := scheme + "://" + target.Host
		setCFHeadersForURL(req, baseURL)
		req.Header.Set("Range", "bytes=0-102399")
	}

	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()

	res.TTFB = float64(time.Since(t0).Microseconds()) / 1000.0
	res.StatusCode = resp.StatusCode

	// Extract Colo from cf-ray header if present
	cfRay := resp.Header.Get("cf-ray")
	if cfRay != "" {
		parts := strings.Split(cfRay, "-")
		if len(parts) >= 2 {
			res.Colo = strings.ToUpper(parts[len(parts)-1])
		}
	}

	if resp.StatusCode >= 400 {
		res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return res
	}

	// Read small body limited to 256KB
	lr := io.LimitReader(resp.Body, 256*1024)
	buf := make([]byte, 32*1024)
	var bytesRead int64
	for {
		n, rErr := lr.Read(buf)
		if n > 0 {
			bytesRead += int64(n)
		}
		if rErr != nil {
			break
		}
	}
	totalDuration := time.Since(t0).Seconds()
	res.BytesRead = bytesRead
	if totalDuration > 0.001 {
		res.Speed = (float64(bytesRead) / 1024.0 / 1024.0) / totalDuration
	}
	res.Success = true
	return res
}

// LightweightHTTPProbe performs a minimal-overhead HTTP check (backward-compatible wrapper).
func LightweightHTTPProbe(ctx context.Context, ip string, port int, testURL, customSNI string) LightweightHTTPProbeResult {
	target := ResolveProbeTarget(ProbeConfig{Profile: NewProfileCustom(testURL, customSNI, port)}, ip, port)
	return LightweightHTTPProbeTarget(ctx, target)
}
