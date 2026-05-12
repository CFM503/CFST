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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

var CloudflareIPv4Ranges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
}

type cidrInfo struct {
	baseIP    uint32
	maxHost   int
	hostBits  int
}

var cidrCache sync.Map // map[string]*cidrInfo

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
	Colo          string  `json:"colo"`
	Score         float64 `json:"score"`
	Jitter        float64 `json:"jitter"`
	Stability     float64 `json:"stability"`
	MinSpeed      float64 `json:"min_speed"`
}

func (n *NodeResult) CalcScore() {
	// 速度分 (45%): YouTube 4K 需要 25-40 Mbps ≈ 3-5 MB/s
	scoreSpeed := 100.0
	if n.DownloadSpeed < 40.0 {
		scoreSpeed = (n.DownloadSpeed / 40.0) * 100.0
	}

	// 延迟分 (15%): 越低越好
	scoreLatency := 100.0 - (n.TCPLatency-30.0)*0.5
	if scoreLatency < 0 {
		scoreLatency = 0
	}

	// 抖动分 (15%): 越低越好，>10ms 开始扣分
	scoreJitter := 100.0 - n.Jitter*2.0
	if scoreJitter < 0 {
		scoreJitter = 0
	}

	// 稳定性分 (25%): 已经是 0-100
	scoreStability := n.Stability

	n.Score = scoreSpeed*0.45 + scoreLatency*0.15 + scoreJitter*0.15 + scoreStability*0.25

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

// ResolveYouTubeCDNIPs 通过 DNS 解析 googlevideo.com 子域名获取 YouTube CDN 节点 IP
func ResolveYouTubeCDNIPs(maxIPs int) []string {
	// YouTube CDN 节点域名前缀（rr{1-9} 表示不同机房，sn-{hash} 表示不同节点）
	prefixes := []string{
		"rr1---sn-", "rr2---sn-", "rr3---sn-", "rr4---sn-",
		"rr5---sn-", "rr6---sn-", "rr7---sn-", "rr8---sn-",
		"rr9---sn-",
	}
	// 常见的 YouTube CDN 节点 hash（不同地区有不同的 hash）
	hashes := []string{
		"a5mlrn76", "a5mekned", "a5meknsd", "a5meknzs",
		"a5mekn7d", "a5meknes", "a5mekney", "a5meknez",
		"a5meknzd", "a5mekn7y", "a5meknsy", "a5mekn7s",
		"a5meknes", "a5mlrnek", "a5mlrnsz", "a5mlrn7y",
		"a5mlrnsy", "a5mlrn7z", "a5mlrnsr", "a5mlrn7k",
		"a5mlrnsk", "a5mlrn7l", "a5mlrnsl", "a5mlrn7m",
		"a5mlrnsm", "a5mlrn7n", "a5mlrnsn", "a5mlrn7p",
		"a5mlrnsp", "a5mlrn7q", "a5mlrnsq", "a5mlrn7r",
		"n4v7sn7s", "n4v7sn7k", "n4v7sn7y", "n4v7sn7z",
		"n4v7snee", "n4v7snes", "n4v7sney", "n4v7snez",
		"n4v7sn7l", "n4v7sn7m", "n4v7sn7n", "n4v7sn7p",
		"cuhx0gbq", "cuhx0gbk", "cuhx0gby", "cuhx0gbz",
		"cuhx0gbs", "cuhx0gbr", "cuhx0gbp", "cuhx0gbn",
		"pouxgp5k", "pouxgp5y", "pouxgp5z", "pouxgp5s",
		"pouxgp5r", "pouxgp5p", "pouxgp5n", "pouxgp5m",
	}

	// SOCKS5 代理时 DNS 也走代理，否则用系统 DNS
	resolver := &net.Resolver{
		PreferGo: true,
		Dial:     proxyDNSDial,
	}

	seen := make(map[string]bool)
	var ips []string

	// 解析 redirector.googlevideo.com（YouTube 的通用重定向节点）
	for _, ip := range resolveHost(resolver, "redirector.googlevideo.com") {
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}

	// 解析不同的 CDN 子域名
	for _, prefix := range prefixes {
		if len(ips) >= maxIPs {
			break
		}
		for _, hash := range hashes {
			if len(ips) >= maxIPs {
				break
			}
			host := prefix + hash + ".googlevideo.com"
			for _, ip := range resolveHost(resolver, host) {
				if !seen[ip] {
					seen[ip] = true
					ips = append(ips, ip)
				}
			}
		}
	}

	if len(ips) > maxIPs {
		ips = ips[:maxIPs]
	}
	return ips
}

func proxyDNSDial(ctx context.Context, network, address string) (net.Conn, error) {
	if proxyDialer != nil {
		// SOCKS5 代理：DNS 查询也走代理（解决 DNS 劫持问题）
		return proxyDialer.Dial(network, address)
	}
	// 直连：使用系统 DNS
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

func resolveHost(resolver *net.Resolver, host string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return nil
	}
	var ips []string
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && ip.To4() != nil {
			ips = append(ips, addr)
		}
	}
	return ips
}

func GenerateIPs(maxScan int, unique bool, ipFile string) []string {
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

	var ips []string
	if unique {
		seen := make(map[string]bool)
		attempts := 0
		maxAttempts := maxScan * 5
		for len(ips) < maxScan && attempts < maxAttempts {
			attempts++
			r := ranges[rand.Intn(len(ranges))]
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

	perRange := maxScan/len(ranges) + 3
	for _, r := range ranges {
		if !strings.Contains(r, "/") {
			ips = append(ips, r)
			continue
		}
		for i := 0; i < perRange; i++ {
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
var clientPool sync.Map // map[string]*http.Client
var proxyDialer proxy.Dialer // nil = direct connection
var proxyURL *url.URL        // for HTTP proxy

func initProxy(addr string) {
	if addr == "" {
		return
	}
	// 自动补全协议前缀
	if !strings.Contains(addr, "://") {
		addr = "socks5://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		fmt.Printf("[!] Invalid proxy: %v\n", err)
		return
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		dialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			fmt.Printf("[!] SOCKS5 proxy error: %v\n", err)
			return
		}
		proxyDialer = dialer
		fmt.Printf("  Proxy: SOCKS5 %s\n", u.Host)
	case "http", "https":
		proxyURL = u
		fmt.Printf("  Proxy: HTTP %s\n", u.Host)
	default:
		fmt.Printf("[!] Unsupported proxy scheme: %s\n", u.Scheme)
	}
}

func makeHTTPClient(ip string, port int, timeout time.Duration) *http.Client {
	key := fmt.Sprintf("%s:%d:%v", ip, port, timeout)
	if v, ok := clientPool.Load(key); ok {
		return v.(*http.Client)
	}
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))

	tr := &http.Transport{
		TLSClientConfig:    sharedTLSConfig,
		MaxIdleConnsPerHost: 10,
	}

	if proxyDialer != nil {
		// SOCKS5 代理：通过代理拨号，DNS 也走代理
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return proxyDialer.Dial(network, addr)
		}
	} else if proxyURL != nil {
		// HTTP 代理：设置 Proxy
		tr.Proxy = http.ProxyURL(proxyURL)
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 2*time.Second)
		}
	} else {
		// 直连
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 2*time.Second)
		}
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
	clientPool.Store(key, client)
	return client
}

func newCFRequest(method, url string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0 Safari/537.36")
	return req, nil
}

func GetColo(ip string, port int) string {
	client := makeHTTPClient(ip, port, 3*time.Second)
	req, err := newCFRequest("GET", "https://speed.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		return "ERR"
	}

	resp, err := client.Do(req)
	if err != nil {
		return "ERR"
	}
	defer resp.Body.Close()

	// Read full body — trace endpoint is tiny (~300 bytes), single read is faster than line-by-line
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "UNK"
	}
	if match := coloRe.FindSubmatch(body); match != nil {
		return string(match[1])
	}
	return "UNK"
}

func CheckBlocked(ip string, port int, testURL string) bool {
	parsedURL, err := url.Parse(testURL)
	if err != nil {
		return false
	}
	host := parsedURL.Hostname()

	client := makeHTTPClient(ip, port, 3*time.Second)

	req, err := newCFRequest("GET", testURL)
	if err != nil {
		return true
	}
	req.Host = host
	req.Header.Set("Connection", "close")

	resp, err := client.Do(req)
	if err != nil {
		return true
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 400
}

func DownloadTest(ip string, port int, threads int, duration int, testURL string) (avgSpeed, minSpeed, stability float64) {
	parsedURL, _ := url.Parse(testURL)
	host := parsedURL.Hostname()

	var totalBytes int64
	var wg sync.WaitGroup

	client := makeHTTPClient(ip, port, 0)

	dur := time.Duration(duration) * time.Second
	sampleInterval := 2 * time.Second
	startGlobal := time.Now()

	// 启动采样协定：每 2 秒记录累计字节数
	var samples []float64 // 每个采样点的累计 MB
	var sampleMu sync.Mutex
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b := atomic.LoadInt64(&totalBytes)
				sampleMu.Lock()
				samples = append(samples, float64(b)/1024.0/1024.0)
				sampleMu.Unlock()
			case <-done:
				return
			}
		}
	}()

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req, err := newCFRequest("GET", testURL)
			if err != nil {
				return
			}
			req.Host = host
			req.Header.Set("Connection", "keep-alive")

			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			buf := make([]byte, 65536)
			for {
				if time.Since(startGlobal) > dur {
					break
				}
				n, err := resp.Body.Read(buf)
				if n > 0 {
					atomic.AddInt64(&totalBytes, int64(n))
				}
				if err != nil {
					break
				}
			}
		}()
	}

	wg.Wait()
	close(done)

	// 最终采样
	finalMB := float64(atomic.LoadInt64(&totalBytes)) / 1024.0 / 1024.0
	sampleMu.Lock()
	samples = append(samples, finalMB)
	sampleMu.Unlock()

	realTime := time.Since(startGlobal).Seconds()
	if realTime < 0.1 {
		realTime = 0.1
	}

	avgSpeed = finalMB / realTime

	// 计算每个区间的瞬时速度
	if len(samples) < 2 {
		return avgSpeed, avgSpeed, 100.0
	}

	var intervalSpeeds []float64
	for i := 1; i < len(samples); i++ {
		dt := sampleInterval.Seconds()
		if i == len(samples)-1 {
			// 最后一个区间可能不足 2 秒
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

	// 最低速度
	minSpeed = intervalSpeeds[0]
	var sum float64
	for _, s := range intervalSpeeds {
		if s < minSpeed {
			minSpeed = s
		}
		sum += s
	}

	// 稳定性 = 100 - 变异系数*100 (变异系数 = 标准差/均值)
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
	cv := stddev / mean // 变异系数
	stability = 100.0 - cv*100.0
	if stability < 0 {
		stability = 0
	}
	if stability > 100 {
		stability = 100
	}

	return avgSpeed, minSpeed, stability
}
