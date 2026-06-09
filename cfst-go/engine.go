package main

import (
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
	IP            string  `json:"ip"`
	Port          int     `json:"port"`
	TCPLatency    float64 `json:"tcp_latency"`
	DownloadSpeed float64 `json:"download_speed"`
	SingleSpeed   float64 `json:"single_speed"`
	LoadLatency   float64 `json:"load_latency"`
	Colo          string  `json:"colo"`
	Score         float64 `json:"score"`
	Jitter        float64 `json:"jitter"`
	Stability     float64 `json:"stability"`
	MinSpeed      float64 `json:"min_speed"`
	PacketLoss    float64 `json:"packet_loss"`
}

func (n *NodeResult) CalcScore() {
	// Speed score (35%): single-stream speed, cap 15 MB/s
	effectiveSpeed := n.DownloadSpeed
	if n.SingleSpeed > 0 {
		effectiveSpeed = n.SingleSpeed
	}
	scoreSpeed := math.Min(effectiveSpeed/15.0*100.0, 100.0)

	// MinSpeed score (20%): floor speed, cap 10 MB/s
	scoreMinSpeed := math.Min(n.MinSpeed/10.0*100.0, 100.0)

	// Latency score (10%): lower is better
	scoreLatency := 100.0 - (n.TCPLatency-30.0)*0.5
	if scoreLatency < 0 {
		scoreLatency = 0
	}

	// Jitter score (10%): >10ms starts penalizing
	scoreJitter := 100.0 - n.Jitter*2.0
	if scoreJitter < 0 {
		scoreJitter = 0
	}

	// Stability score (25%)
	scoreStability := n.Stability

	n.Score = scoreSpeed*0.35 + scoreMinSpeed*0.20 + scoreLatency*0.10 +
		scoreJitter*0.10 + scoreStability*0.25

	if n.Colo != "UNK" && n.Colo != "ERR" && n.Colo != "" {
		n.Score += 5.0
	}
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

// SingleStreamTest measures single-connection download speed.
// Returns avgSpeed (MB/s), minSpeed (MB/s), stability (0-100).
func SingleStreamTest(ctx context.Context, ip string, port int, duration int, testURL string, customSNI string,
	progressCallback func(LiveProgress)) (avgSpeed, minSpeed, stability float64) {

	parsedURL, err := url.Parse(testURL)
	if err != nil {
		return 0, 0, 0
	}
	host := parsedURL.Hostname()

	// Set SNI to the actual domain so CF routes correctly
	sni := host
	if customSNI != "" {
		sni = customSNI
	} else if strings.Contains(testURL, "speed.cloudflare.com") {
		sni = ""
	}
	client := makeHTTPClient(ip, port, sni)
	if tr, ok := client.Transport.(*http.Transport); ok {
		defer tr.CloseIdleConnections()
	}

	dur := time.Duration(duration) * time.Second
	downloadCtx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	req, err := newCFRequestWithContext(downloadCtx, "GET", testURL)
	if err != nil {
		return 0, 0, 0
	}
	req.Host = host
	req.Header.Set("Connection", "keep-alive")

	if !strings.Contains(testURL, "speed.cloudflare.com") {
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

	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return 0, 0, 0
	}

	startGlobal := time.Now()
	var totalBytes int64
	sampleInterval := 2 * time.Second
	var samples []float64
	var sampleMu sync.Mutex

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b := atomic.LoadInt64(&totalBytes)
				mb := float64(b) / 1024.0 / 1024.0
				sampleMu.Lock()
				samples = append(samples, mb)
				sampleMu.Unlock()
				elapsed := time.Since(startGlobal).Seconds()
				if progressCallback != nil {
					progressCallback(LiveProgress{
						IP:       ip,
						Bytes:    b,
						Speed:    mb / elapsed,
						Elapsed:  elapsed,
						Duration: float64(duration),
					})
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

	finalMB := float64(atomic.LoadInt64(&totalBytes)) / 1024.0 / 1024.0
	sampleMu.Lock()
	samples = append(samples, finalMB)
	sampleMu.Unlock()

	realTime := time.Since(startGlobal).Seconds()
	if realTime < 0.1 {
		return 0, 0, 0
	}

	avgSpeed = finalMB / realTime

	if len(samples) < 2 {
		return avgSpeed, avgSpeed, 100.0
	}

	var intervalSpeeds []float64
	for i := 1; i < len(samples); i++ {
		dt := sampleInterval.Seconds()
		if i == len(samples)-1 {
			elapsed := realTime - float64(i-1)*sampleInterval.Seconds()
			if elapsed > 0.1 {
				dt = elapsed
			}
		}
		speed := (samples[i] - samples[i-1]) / dt
		if speed > 0 {
			intervalSpeeds = append(intervalSpeeds, speed)
		}
	}

	if len(intervalSpeeds) == 0 {
		return avgSpeed, avgSpeed, 100.0
	}

	minSpeed = intervalSpeeds[0]
	var sum float64
	for _, s := range intervalSpeeds {
		if s < minSpeed {
			minSpeed = s
		}
		sum += s
	}

	mean := sum / float64(len(intervalSpeeds))
	if mean < 0.01 {
		return avgSpeed, minSpeed, 0.0
	}
	var variance float64
	for _, s := range intervalSpeeds {
		diff := s - mean
		variance += diff * diff
	}
	variance /= float64(len(intervalSpeeds))
	stddev := math.Sqrt(variance)
	cv := stddev / mean
	stability = 100.0 - cv*100.0
	if stability < 0 {
		stability = 0
	}
	if stability > 100 {
		stability = 100
	}

	return avgSpeed, minSpeed, stability
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
	return float64(time.Since(start).Microseconds()) / 1000.0
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
