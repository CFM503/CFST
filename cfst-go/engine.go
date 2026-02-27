package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"math/big"
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
)

var CloudflareIPv4Ranges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
}

type NodeResult struct {
	IP            string  `json:"ip"`
	Port          int     `json:"port"`
	TCPLatency    float64 `json:"tcp_latency"`
	DownloadSpeed float64 `json:"download_speed"`
	Colo          string  `json:"colo"`
	Score         float64 `json:"score"`
}

func (n *NodeResult) CalcScore() {
	scoreSpeed := 100.0
	if n.DownloadSpeed < 40.0 {
		scoreSpeed = (n.DownloadSpeed / 40.0) * 100.0
	}
	scoreLatency := 100.0 - (n.TCPLatency-30.0)*0.5
	if scoreLatency < 0 {
		scoreLatency = 0
	}
	bonus := 0.0
	if n.Colo != "UNK" && n.Colo != "ERR" && n.Colo != "" {
		bonus = 5.0
	}
	n.Score = scoreSpeed*0.8 + scoreLatency*0.2 + bonus
}

func randIPFromCIDR(cidr string) string {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ones, bits := ipNet.Mask.Size()
	hostBits := bits - ones
	if hostBits <= 2 {
		return ipNet.IP.String()
	}
	maxHost := (1 << hostBits) - 2
	offset := rand.Intn(maxHost) + 1

	ip := make(net.IP, len(ipNet.IP))
	copy(ip, ipNet.IP)

	ipInt := big.NewInt(0).SetBytes(ip.To4())
	ipInt.Add(ipInt, big.NewInt(int64(offset)))
	b := ipInt.Bytes()
	result := net.IP(make([]byte, 4))
	copy(result[4-len(b):], b)
	return result.String()
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
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), timeout)
	if err != nil {
		return 0
	}
	conn.Close()
	return float64(time.Since(start).Microseconds()) / 1000.0
}

var coloRe = regexp.MustCompile(`colo=([A-Z]+)`)

func GetColo(ip string, port int) string {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
			},
			TLSClientConfig: &tls.Config{
				ServerName:         "speed.cloudflare.com",
				InsecureSkipVerify: true,
			},
		},
		Timeout: 3 * time.Second,
	}

	req, _ := http.NewRequest("GET", "https://speed.cloudflare.com/cdn-cgi/trace", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return "ERR"
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if match := coloRe.FindStringSubmatch(line); match != nil {
			return match[1]
		}
	}
	return "UNK"
}

func CheckBlocked(ip string, port int, testURL string) bool {
	parsedURL, err := url.Parse(testURL)
	if err != nil {
		return false
	}
	host := parsedURL.Hostname()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
			},
			TLSClientConfig: &tls.Config{
				ServerName:         host,
				InsecureSkipVerify: true,
			},
		},
		Timeout: 3 * time.Second,
	}

	req, _ := http.NewRequest("GET", testURL, nil)
	req.Host = host
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0 Safari/537.36")
	req.Header.Set("Connection", "close")

	resp, err := client.Do(req)
	if err != nil {
		return true
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden || resp.StatusCode == 400 || resp.StatusCode == 404 || resp.StatusCode == 403 {
		return true
	}
	// Any other 5xx or unhandled could also mean blocked, but we mainly catch WAF rules
	if resp.StatusCode >= 400 {
		return true
	}
	return false
}

func DownloadTest(ip string, port int, threads int, duration int, testURL string) float64 {
	parsedURL, _ := url.Parse(testURL)
	host := parsedURL.Hostname()

	var totalBytes int64
	var wg sync.WaitGroup

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), 3*time.Second)
			},
			TLSClientConfig: &tls.Config{
				ServerName:         host,
				InsecureSkipVerify: true,
			},
			MaxIdleConnsPerHost: threads,
		},
	}

	startGlobal := time.Now()
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req, _ := http.NewRequest("GET", testURL, nil)
			req.Host = host
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0 Safari/537.36")
			req.Header.Set("Connection", "keep-alive")

			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			buf := make([]byte, 65536)
			dur := float64(duration)
			for {
				if time.Since(startGlobal).Seconds() > dur {
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
	realTime := time.Since(startGlobal).Seconds()
	if realTime < 0.1 {
		realTime = 0.1
	}

	return (float64(totalBytes) / 1024.0 / 1024.0) / realTime
}
