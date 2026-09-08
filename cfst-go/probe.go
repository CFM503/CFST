package main

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ProbeProfileType string

const (
	ProfileCFST     ProbeProfileType = "CFST"
	ProfileGOWAYWSS ProbeProfileType = "GOWAY-WSS"
	ProfileCustom   ProbeProfileType = "CUSTOM"
)

// ProbeProfile defines target parameters for versatile network quality testing (CFST, GOWAY-WSS, CUSTOM).
type ProbeProfile struct {
	Type     ProbeProfileType `json:"type"`
	IP       string           `json:"ip,omitempty"`
	Port     int              `json:"port"`
	SNI      string           `json:"sni"`
	Host     string           `json:"host"`
	Path     string           `json:"path"`
	TestURL  string           `json:"test_url"`
	Protocol string           `json:"protocol"` // e.g. "https", "wss"
}

// NewProfileCFST returns Cloudflare Official probe profile (default).
func NewProfileCFST() ProbeProfile {
	return ProbeProfile{
		Type:     ProfileCFST,
		Port:     443,
		SNI:      "speed.cloudflare.com",
		Host:     "speed.cloudflare.com",
		Path:     "/__down",
		TestURL:  "https://speed.cloudflare.com/__down?bytes=500000000",
		Protocol: "https",
	}
}

// NewProfileGOWAYWSS returns GOWAY WSS Handshake probe profile.
func NewProfileGOWAYWSS(host, path, sni string, port int) ProbeProfile {
	if port <= 0 {
		port = 443
	}
	if path == "" {
		path = "/pyway"
	}
	return ProbeProfile{
		Type:     ProfileGOWAYWSS,
		Port:     port,
		SNI:      sni,
		Host:     host,
		Path:     path,
		TestURL:  "https://speed.cloudflare.com/__down?bytes=500000000",
		Protocol: "wss",
	}
}

// NewProfileCustom returns custom user-configured VPS URL probe profile.
func NewProfileCustom(customURL, sni string, port int) ProbeProfile {
	path := "/"
	protocol := "https"
	hostname := sni
	if customURL != "" {
		if u, err := url.Parse(customURL); err == nil {
			if u.Hostname() != "" {
				hostname = u.Hostname()
			}
			if u.Port() != "" {
				if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 {
					port = p
				}
			}
			if u.RequestURI() != "" {
				path = u.RequestURI()
			}
			if u.Scheme != "" {
				protocol = u.Scheme
			}
		}
	}
	if port <= 0 {
		if protocol == "http" {
			port = 80
		} else {
			port = 443
		}
	}
	if sni == "" {
		sni = hostname
	}
	if strings.Contains(sni, ":") {
		sni = strings.Split(sni, ":")[0]
	}

	host := hostname
	if port != 443 && port != 80 && hostname != "" && !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", hostname, port)
	}

	return ProbeProfile{
		Type:     ProfileCustom,
		Port:     port,
		SNI:      sni,
		Host:     host,
		Path:     path,
		TestURL:  customURL,
		Protocol: protocol,
	}
}

// ResolvedProbeTarget defines a normalized and unified target descriptor
// ensuring all probe layers (L1~L4) execute with strictly consistent parameters.
type ResolvedProbeTarget struct {
	ProfileType ProbeProfileType `json:"profile_type"`
	IP          string           `json:"ip"`
	Port        int              `json:"port"`
	SNI         string           `json:"sni"`
	Host        string           `json:"host"`
	Path        string           `json:"path"`
	URL         string           `json:"url"`
	Protocol    string           `json:"protocol"`
}

// NormalizeProbeConfig reconciles cfg.Profile as the single source of truth,
// ensuring legacy fields (URL, SNI, WSSHost, Port) stay in sync and isolating WSS in CFST mode.
func NormalizeProbeConfig(cfg *ProbeConfig) {
	if cfg == nil {
		return
	}

	if cfg.Profile.Type == "" {
		cfg.Profile = NewProfileCFST()
	}

	switch cfg.Profile.Type {
	case ProfileCFST:
		if cfg.Profile.TestURL == "" {
			cfg.Profile.TestURL = "https://speed.cloudflare.com/__down?bytes=500000000"
		}
		if cfg.Profile.SNI == "" {
			cfg.Profile.SNI = "speed.cloudflare.com"
		}
		if cfg.Profile.Host == "" {
			cfg.Profile.Host = cfg.Profile.SNI
		}
		if cfg.Profile.Path == "" {
			cfg.Profile.Path = "/__down"
		}
		if cfg.Profile.Protocol == "" {
			cfg.Profile.Protocol = "https"
		}
		if cfg.Profile.Port <= 0 {
			cfg.Profile.Port = 443
		}
		// In CFST mode, WSSHost must NOT participate
		cfg.WSSHost = ""

	case ProfileGOWAYWSS:
		if cfg.Profile.Path == "" {
			cfg.Profile.Path = "/pyway"
		}
		if cfg.Profile.Protocol == "" {
			cfg.Profile.Protocol = "wss"
		}
		if cfg.Profile.Port <= 0 {
			cfg.Profile.Port = 443
		}
		if cfg.Profile.TestURL == "" {
			cfg.Profile.TestURL = "https://speed.cloudflare.com/__down?bytes=500000000"
		}
		cfg.WSSHost = cfg.Profile.Host

	case ProfileCustom:
		if cfg.Profile.TestURL != "" {
			if u, err := url.Parse(cfg.Profile.TestURL); err == nil && u.Hostname() != "" {
				if u.Port() != "" {
					if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 {
						cfg.Profile.Port = p
					}
				}
				if cfg.Profile.Host == "" {
					cfg.Profile.Host = u.Hostname()
				}
				if cfg.Profile.SNI == "" {
					cfg.Profile.SNI = u.Hostname()
				}
				if cfg.Profile.Path == "" {
					cfg.Profile.Path = u.RequestURI()
				}
				if cfg.Profile.Protocol == "" && u.Scheme != "" {
					cfg.Profile.Protocol = u.Scheme
				}
			}
		}
		if cfg.Profile.Port <= 0 {
			if cfg.Profile.Protocol == "http" {
				cfg.Profile.Port = 80
			} else {
				cfg.Profile.Port = 443
			}
		}
		if cfg.Profile.Protocol == "" {
			cfg.Profile.Protocol = "https"
		}
		if cfg.Profile.Path == "" {
			cfg.Profile.Path = "/"
		}
		if strings.Contains(cfg.Profile.SNI, ":") {
			cfg.Profile.SNI = strings.Split(cfg.Profile.SNI, ":")[0]
		}
		if cfg.Profile.Port != 443 && cfg.Profile.Port != 80 && cfg.Profile.Host != "" && !strings.Contains(cfg.Profile.Host, ":") {
			cfg.Profile.Host = fmt.Sprintf("%s:%d", cfg.Profile.Host, cfg.Profile.Port)
		}
		if cfg.Profile.Protocol == "wss" {
			cfg.WSSHost = cfg.Profile.Host
		} else {
			cfg.WSSHost = ""
		}
	}

	// Synchronize legacy fields from Profile (Profile is the single source of truth!)
	cfg.URL = cfg.Profile.TestURL
	cfg.SNI = cfg.Profile.SNI
	cfg.Port = cfg.Profile.Port

	if cfg.L3ProbeCycle < 1 {
		cfg.L3ProbeCycle = 3
	}
	if cfg.FullSpeedCycle < 1 {
		cfg.FullSpeedCycle = 60
	}
	cfg.SpeedTestCycle = cfg.FullSpeedCycle
	if cfg.MaxSpeedTests < 1 {
		cfg.MaxSpeedTests = 1
	}
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 2
	}
	if cfg.DiscoveryInterval <= 0 {
		cfg.DiscoveryInterval = 60 * time.Minute
	}
	if cfg.DiscoveryScanCount <= 0 {
		cfg.DiscoveryScanCount = 200
	}
}

// ResolveProbeTarget returns a ResolvedProbeTarget for an IP/port based strictly on cfg.Profile.
func ResolveProbeTarget(cfg ProbeConfig, ip string, port int) ResolvedProbeTarget {
	NormalizeProbeConfig(&cfg)
	targetPort := port
	if targetPort <= 0 {
		targetPort = cfg.Profile.Port
	}
	if targetPort <= 0 {
		targetPort = 443
	}

	host := cfg.Profile.Host
	if cfg.Profile.Type == ProfileCustom {
		if targetPort != 443 && targetPort != 80 && host != "" && !strings.Contains(host, ":") {
			host = fmt.Sprintf("%s:%d", host, targetPort)
		}
	}

	sni := cfg.Profile.SNI
	if strings.Contains(sni, ":") {
		sni = strings.Split(sni, ":")[0]
	}

	return ResolvedProbeTarget{
		ProfileType: cfg.Profile.Type,
		IP:          ip,
		Port:        targetPort,
		SNI:         sni,
		Host:        host,
		Path:        cfg.Profile.Path,
		URL:         cfg.Profile.TestURL,
		Protocol:    cfg.Profile.Protocol,
	}
}

// ProbeConfig holds parameters controlling tiered background probing.
type ProbeConfig struct {
	Profile            ProbeProfile  `json:"profile"`
	ActiveInterval     time.Duration `json:"active_interval"`      // High frequency for active routes
	StandbyInterval    time.Duration `json:"standby_interval"`     // Medium frequency for standby routes
	CandidateInterval  time.Duration `json:"candidate_interval"`   // Low frequency for candidate pool
	FailedInterval     time.Duration `json:"failed_interval"`      // Periodic recovery checks for failed routes
	MaxConcurrent      int           `json:"max_concurrent"`       // Concurrency limit for background ping/wss
	MaxSpeedTests      int           `json:"max_speed_tests"`      // Strict limit (usually 1) for concurrent speed tests
	QuickDuration      int           `json:"quick_duration"`       // Duration in seconds for L3 quick speed test
	FullDuration       int           `json:"full_duration"`        // Duration in seconds for L4 full speed test
	L3ProbeCycle       int           `json:"l3_probe_cycle"`       // Lightweight HTTP check every N cycles (e.g. 3)
	FullSpeedCycle     int           `json:"full_speed_cycle"`     // Full speed test calibration cycle (e.g. 60)
	SpeedTestCycle     int           `json:"speed_test_cycle"`     // Legacy alias for FullSpeedCycle
	DiscoveryEnabled   bool          `json:"discovery_enabled"`    // Periodic background Cloudflare IP discovery
	DiscoveryInterval  time.Duration `json:"discovery_interval"`   // Interval between discovery passes (default 60m)
	DiscoveryScanCount int           `json:"discovery_scan_count"` // Number of random IPs scanned per pass (default 200)
	WSSHost            string        `json:"wss_host"`
	SNI                string        `json:"sni"`
	URL                string        `json:"url"`
	Port               int           `json:"port"`
}

func DefaultProbeConfig() ProbeConfig {
	prof := NewProfileCFST()
	cfg := ProbeConfig{
		Profile:            prof,
		ActiveInterval:     10 * time.Second,
		StandbyInterval:    30 * time.Second,
		CandidateInterval:  180 * time.Second,
		FailedInterval:     60 * time.Second,
		MaxConcurrent:      4,
		MaxSpeedTests:      1,
		QuickDuration:      3,
		FullDuration:       10,
		L3ProbeCycle:       3,  // L3 lightweight 100KB probe every 3 cycles (~30s on Active)
		FullSpeedCycle:     60, // L4 full speed calibration every 60 cycles (~10m on Active)
		SpeedTestCycle:     60,
		DiscoveryEnabled:   true,
		DiscoveryInterval:  60 * time.Minute,
		DiscoveryScanCount: 200,
		WSSHost:            "", // isolated from CFST mode!
		SNI:                prof.SNI,
		URL:                prof.TestURL,
		Port:               prof.Port,
	}
	NormalizeProbeConfig(&cfg)
	return cfg
}

// LayeredProbeResult contains metrics produced by a multi-layer probe pass.
type LayeredProbeResult struct {
	Success              bool
	RTT                  float64
	PacketLoss           float64
	Jitter               float64
	HandshakeSuccess     bool
	SingleSpeed          float64
	DownloadSpeed        float64
	MinSpeed             float64
	P10Speed             float64
	MedianSpeed          float64
	Stability            float64
	CV                   float64
	StallCount           int
	ZeroSpeedIntervals   int
	TotalStallDuration   float64
	LongestStallDuration float64
	StallRate            float64
	LoadLatency          float64
	Colo                 string
	Error                string
	L3Executed           bool
	L4Executed           bool
}

// ProbeScheduler runs layered active background testing on managed routes.
type ProbeScheduler struct {
	mu               sync.RWMutex
	store            *RouteStore
	cfg              ProbeConfig
	speedSem         chan struct{} // limits concurrent bandwidth-heavy speed tests
	probeSem         chan struct{} // limits lightweight concurrent pings/handshakes
	cycleCount       sync.Map      // route IP -> int64 cycle count
	inFlight         sync.Map      // route IP -> struct{} per-IP probe concurrency de-duplication
	running          atomic.Bool
	stopCh           chan struct{}
	probeTrigger     chan string
	probeExec        func(ctx context.Context, target ResolvedProbeTarget, cfg ProbeConfig, runL3 bool, runFullSpeed bool) LayeredProbeResult
	nowFunc          func() time.Time
	onStartGoroutine func()
	onExitGoroutine  func()
}

var GlobalProbeScheduler = NewProbeScheduler(GlobalRouteStore, DefaultProbeConfig())

func NewProbeScheduler(store *RouteStore, cfg ProbeConfig) *ProbeScheduler {
	NormalizeProbeConfig(&cfg)
	return &ProbeScheduler{
		store:        store,
		cfg:          cfg,
		speedSem:     make(chan struct{}, cfg.MaxSpeedTests),
		probeSem:     make(chan struct{}, cfg.MaxConcurrent),
		stopCh:       make(chan struct{}),
		probeTrigger: make(chan string, 100),
	}
}

func (ps *ProbeScheduler) UpdateConfig(cfg ProbeConfig) {
	NormalizeProbeConfig(&cfg)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.cfg = cfg
}

func (ps *ProbeScheduler) GetConfig() ProbeConfig {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.cfg
}

// ExecuteLayeredProbe executes L1 to L5 sequentially on an IP, strictly driven by cfg.Profile via ResolvedProbeTarget.
func (ps *ProbeScheduler) ExecuteLayeredProbe(ctx context.Context, ip string, port int, runL3 bool, runFullSpeed bool) LayeredProbeResult {
	cfg := ps.GetConfig()
	target := ResolveProbeTarget(cfg, ip, port)
	return ps.ExecuteLayeredProbeWithSnapshot(ctx, target, cfg, runL3, runFullSpeed)
}

// ExecuteLayeredProbeWithSnapshot executes L1 to L5 sequentially with a fixed target and config snapshot.
func (ps *ProbeScheduler) ExecuteLayeredProbeWithSnapshot(ctx context.Context, target ResolvedProbeTarget, cfg ProbeConfig, runL3 bool, runFullSpeed bool) LayeredProbeResult {
	res := LayeredProbeResult{Success: false}

	// -------------------------------------------------------------
	// Layer 1: TCP Ping (5 pings, jitter & packet loss)
	// -------------------------------------------------------------
	pingCount := 5
	lats := make([]float64, 0, pingCount)
	for i := 0; i < pingCount; i++ {
		if ctx.Err() != nil {
			res.Error = "Context cancelled"
			return res
		}
		lat := TCPPing(target.IP, target.Port, 1500*time.Millisecond)
		if lat > 0 {
			lats = append(lats, lat)
		}
		if i < pingCount-1 {
			time.Sleep(30 * time.Millisecond)
		}
	}

	if len(lats) == 0 {
		res.Error = "All TCP pings timed out"
		res.PacketLoss = 1.0
		return res
	}

	res.PacketLoss = float64(pingCount-len(lats)) / float64(pingCount)
	var sum float64
	for _, l := range lats {
		sum += l
	}
	res.RTT = sum / float64(len(lats))

	if len(lats) > 1 {
		var variance float64
		for _, l := range lats {
			diff := l - res.RTT
			variance += diff * diff
		}
		res.Jitter = math.Sqrt(variance / float64(len(lats)))
	}

	// Drop immediately if ping loss is severe (>60%)
	if res.PacketLoss >= 0.60 {
		res.Error = "Severe packet loss on Layer 1"
		return res
	}

	// -------------------------------------------------------------
	// Layer 2: L2 Connectivity Check (Profile-Driven)
	// -------------------------------------------------------------
	switch target.ProfileType {
	case ProfileCFST:
		// ProfileCFST: Standard HTTPS / TLS connectivity check.
		// NEVER run GOWAY WSS Handshake or request /pyway in CFST mode.
		ok, _, _, colo, err := HTTPSConnectivityCheck(ctx, target.IP, target.Port, target.SNI, target.Host, target.URL, 3*time.Second)
		if !ok {
			res.HandshakeSuccess = false
			if err != nil {
				res.Error = "HTTPS connectivity check failed: " + err.Error()
			} else {
				res.Error = "HTTPS connectivity check failed"
			}
			return res
		}
		res.HandshakeSuccess = true
		if colo != "" {
			res.Colo = colo
		}

	case ProfileGOWAYWSS:
		// ProfileGOWAYWSS: GOWAY WSS Handshake with configurable Host, SNI, and Path from Profile.
		wssRes := WSSHandshakeCheckDetailed(target.IP, target.Port, target.SNI, target.Host, target.Path, 3*time.Second)
		if !wssRes.Success {
			res.HandshakeSuccess = false
			res.Error = fmt.Sprintf("GOWAY WSS handshake failed (%s: %s)", wssRes.ErrorStage, wssRes.ErrorMessage)
			return res
		}
		res.HandshakeSuccess = true

	case ProfileCustom:
		// ProfileCustom: Protocol-driven check
		if target.Protocol == "wss" {
			wssRes := WSSHandshakeCheckDetailed(target.IP, target.Port, target.SNI, target.Host, target.Path, 3*time.Second)
			if !wssRes.Success {
				res.HandshakeSuccess = false
				res.Error = fmt.Sprintf("Custom WSS handshake failed (%s: %s)", wssRes.ErrorStage, wssRes.ErrorMessage)
				return res
			}
			res.HandshakeSuccess = true
		} else {
			ok, _, _, colo, err := HTTPSConnectivityCheck(ctx, target.IP, target.Port, target.SNI, target.Host, target.URL, 3*time.Second)
			if !ok {
				res.HandshakeSuccess = false
				if err != nil {
					res.Error = "Custom HTTP(S) check failed: " + err.Error()
				} else {
					res.Error = "Custom HTTP(S) check failed"
				}
				return res
			}
			res.HandshakeSuccess = true
			if colo != "" {
				res.Colo = colo
			}
		}
	}

	// -------------------------------------------------------------
	// Layer 3: Lightweight HTTP / HTTPS Probe (~100KB payload)
	// -------------------------------------------------------------
	if runL3 && !runFullSpeed {
		l3Res := LightweightHTTPProbeTarget(ctx, target)
		if !l3Res.Success {
			res.Error = "L3 HTTP probe failed: " + l3Res.Error
			return res
		}
		res.SingleSpeed = l3Res.Speed
		res.DownloadSpeed = l3Res.Speed
		if l3Res.Colo != "" {
			res.Colo = l3Res.Colo
		}
		if l3Res.TTFB > 0 {
			res.LoadLatency = l3Res.TTFB
		}
		res.L3Executed = true
	}

	// -------------------------------------------------------------
	// Layer 4: Full Speed Test (Bandwidth-throttled calibration)
	// -------------------------------------------------------------
	if runFullSpeed {
		// Acquire speed semaphore to strictly prevent bandwidth congestion
		select {
		case ps.speedSem <- struct{}{}:
			defer func() { <-ps.speedSem }()
		case <-ctx.Done():
			res.Error = "Cancelled waiting for speed test slot"
			return res
		}

		duration := cfg.FullDuration
		if duration < 2 {
			duration = 5
		}

		sm := SingleStreamTestDetailed(ctx, target, duration, nil, res.RTT, res.Jitter, res.PacketLoss)
		res.SingleSpeed = sm.AverageSpeed
		res.DownloadSpeed = sm.AverageSpeed
		res.MinSpeed = sm.MinSpeed
		res.P10Speed = sm.P10Speed
		res.MedianSpeed = sm.MedianSpeed
		res.Stability = sm.Stability
		res.CV = sm.CoefficientOfVariation
		res.StallCount = sm.StallCount
		res.ZeroSpeedIntervals = sm.ZeroSpeedIntervals
		res.TotalStallDuration = sm.TotalStallDuration
		res.LongestStallDuration = sm.LongestStallDuration
		res.StallRate = sm.StallRate
		res.L4Executed = true

		if sm.AverageSpeed <= 0 && sm.MinSpeed <= 0 {
			res.Error = "Speed test returned 0 MB/s"
			return res
		}

		// Detect Colo if unknown and CF URL
		if res.Colo == "" && strings.Contains(target.URL, "speed.cloudflare.com") {
			res.Colo = GetColo(target.IP, target.Port)
		}

		// Layer 5: Load Latency (optional, only for official CF endpoint)
		if !isCustomURL(target.URL) {
			res.LoadLatency = MeasureLoadLatency(target.IP, target.Port)
		}
	}

	res.Success = true
	return res
}

// ProbeOnce executes a probe pass for a single route and updates the store.
func (ps *ProbeScheduler) incCycle(ip string) int64 {
	val, _ := ps.cycleCount.LoadOrStore(ip, int64(0))
	cycle := val.(int64) + 1
	ps.cycleCount.Store(ip, cycle)
	return cycle
}

func (ps *ProbeScheduler) ProbeOnce(ctx context.Context, ip string, forceSpeed bool) (*RouteMetrics, error) {
	// 1. In-flight per-IP probe de-duplication:
	// Only one ProbeOnce can run per IP at any time.
	if _, loaded := ps.inFlight.LoadOrStore(ip, struct{}{}); loaded {
		if m, exists := ps.store.GetMetrics(ip); exists {
			snapshot := m
			return &snapshot, nil
		}
		return nil, fmt.Errorf("probe already in progress for %s", ip)
	}
	defer ps.inFlight.Delete(ip)

	// 2. Atomic configuration snapshot: single synchronized read, preventing torn or mixed state
	cfg := ps.GetConfig()

	port := cfg.Port
	tier := TierCandidate
	existingColo := ""
	recM, exists := ps.store.GetMetrics(ip)
	if exists {
		if recM.Port > 0 {
			port = recM.Port
		}
		tier = recM.Tier
		existingColo = recM.Colo
	}

	// 3. Determine if speed test is scheduled using snapshot cfg
	cycle := ps.incCycle(ip)

	runL3 := false
	runFullSpeed := false
	if forceSpeed {
		runL3 = true
		runFullSpeed = true
	} else {
		l3Cycle := cfg.L3ProbeCycle
		if l3Cycle < 1 {
			l3Cycle = 3
		}
		fullCycle := cfg.FullSpeedCycle
		if fullCycle < 1 {
			fullCycle = 60
		}

		switch tier {
		case TierActive:
			// Active route: L1/L2 every cycle (10s)
			// L3 lightweight 100KB check every L3ProbeCycle (default 3 cycles = ~30s)
			if cycle == 1 || cycle%int64(l3Cycle) == 0 {
				runL3 = true
			}
			// L4 full speed calibration only every FullSpeedCycle (default 60 cycles = ~10m)
			if cycle%int64(fullCycle) == 0 {
				runFullSpeed = true
			}
		case TierStandby:
			// Standby route: lower frequency
			if cycle == 1 || cycle%int64(l3Cycle*2) == 0 {
				runL3 = true
			}
			if cycle%int64(fullCycle*2) == 0 {
				runFullSpeed = true
			}
		case TierCandidate:
			// Candidate: low frequency
			if cycle == 1 || cycle%int64(l3Cycle*4) == 0 {
				runL3 = true
			}
			runFullSpeed = false
		case TierFailed:
			// Failed routes: ONLY L1/L2 recovery checks, ZERO speed tests!
			runL3 = false
			runFullSpeed = false
		}
	}

	target := ResolveProbeTarget(cfg, ip, port)
	var result LayeredProbeResult
	if ps.probeExec != nil {
		result = ps.probeExec(ctx, target, cfg, runL3, runFullSpeed)
	} else {
		result = ps.ExecuteLayeredProbeWithSnapshot(ctx, target, cfg, runL3, runFullSpeed)
	}

	durationSec := 0.0
	if runFullSpeed {
		d := cfg.FullDuration
		if d < 2 {
			d = 5
		}
		durationSec = float64(d)
	} else if runL3 {
		durationSec = 1.0
	}

	now := time.Now()
	if ps.nowFunc != nil {
		now = ps.nowFunc()
	}
	m := RouteMetrics{
		ID:                   GenerateRouteID(ip, port),
		IP:                   ip,
		Port:                 port,
		Colo:                 result.Colo,
		Tier:                 tier,
		RTT:                  result.RTT,
		PacketLoss:           result.PacketLoss,
		Jitter:               result.Jitter,
		HandshakeSuccess:     result.HandshakeSuccess,
		DownloadSpeed:        result.SingleSpeed,
		SingleSpeed:          result.SingleSpeed,
		P10Speed:             result.P10Speed,
		MedianSpeed:          result.MedianSpeed,
		MinSpeed:             result.MinSpeed,
		Stability:            result.Stability,
		CV:                   result.CV,
		StallCount:           result.StallCount,
		ZeroSpeedIntervals:   result.ZeroSpeedIntervals,
		TotalStallDuration:   result.TotalStallDuration,
		LongestStallDuration: result.LongestStallDuration,
		StallRate:            result.StallRate,
		DurationSeconds:      durationSec,
		LoadLatency:          result.LoadLatency,
		LastTested:           now,
		Timestamp:            now,
		ConsecutiveFails:     0,
		ConsecutiveSuccess:   0,
	}

	if m.Colo == "" {
		m.Colo = existingColo
	}
	if exists {
		m.ConsecutiveFails = recM.ConsecutiveFails
		m.ConsecutiveSuccess = recM.ConsecutiveSuccess
		m.ConsecutiveDegraded = recM.ConsecutiveDegraded
		m.Health = recM.Health
		// If full speed test wasn't run on this cycle, handle speeds
		if !runFullSpeed {
			if runL3 {
				// L3 updated single speed / download speed from real lightweight HTTP probe
				m.DownloadSpeed = result.SingleSpeed
				m.SingleSpeed = result.SingleSpeed
				if result.LoadLatency > 0 {
					m.LoadLatency = result.LoadLatency
				} else {
					m.LoadLatency = recM.LoadLatency
				}
				// Preserve existing P10/Median/Min/Stability/CV/Stall from previous history
				m.P10Speed = recM.P10Speed
				m.MedianSpeed = recM.MedianSpeed
				m.MinSpeed = recM.MinSpeed
				m.Stability = recM.Stability
				m.CV = recM.CV
				m.StallCount = recM.StallCount
				m.ZeroSpeedIntervals = recM.ZeroSpeedIntervals
				m.TotalStallDuration = recM.TotalStallDuration
				m.LongestStallDuration = recM.LongestStallDuration
				m.StallRate = recM.StallRate
			} else {
				m.DownloadSpeed = recM.DownloadSpeed
				m.SingleSpeed = recM.SingleSpeed
				m.P10Speed = recM.P10Speed
				m.MedianSpeed = recM.MedianSpeed
				m.MinSpeed = recM.MinSpeed
				m.Stability = recM.Stability
				m.CV = recM.CV
				m.StallCount = recM.StallCount
				m.ZeroSpeedIntervals = recM.ZeroSpeedIntervals
				m.TotalStallDuration = recM.TotalStallDuration
				m.LongestStallDuration = recM.LongestStallDuration
				m.StallRate = recM.StallRate
				m.LoadLatency = recM.LoadLatency
			}
		}
	}

	ps.store.RecordProbeResult(m, result.Success)
	if updated, exists := ps.store.GetMetrics(ip); exists {
		snapshot := updated
		return &snapshot, nil
	}
	snapshot := m
	return &snapshot, nil
}

// Start launches the background probing daemon.
func (ps *ProbeScheduler) Start(ctx context.Context) {
	ps.mu.Lock()
	if ps.running.Load() {
		ps.mu.Unlock()
		return // already running
	}
	ps.running.Store(true)
	stopCh := make(chan struct{})
	ps.stopCh = stopCh
	ps.mu.Unlock()

	go func(myStopCh chan struct{}, myCtx context.Context) {
		if ps.onStartGoroutine != nil {
			ps.onStartGoroutine()
		}
		defer func() {
			if ps.onExitGoroutine != nil {
				ps.onExitGoroutine()
			}
		}()

		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-myStopCh:
				return
			case <-myCtx.Done():
				// Only clear running state if this generation is still current.
				// Prevents a stale ctx-cancel from clobbering a newer Start().
				ps.mu.Lock()
				if ps.stopCh == myStopCh {
					ps.running.Store(false)
					ps.stopCh = nil
				}
				ps.mu.Unlock()
				return
			case ip := <-ps.probeTrigger:
				go func(targetIP string) {
					ps.ProbeOnce(myCtx, targetIP, true)
				}(ip)
			case now := <-ticker.C:
				ps.evaluateAndSchedule(myCtx, now)
			}
		}
	}(stopCh, ctx)
}

// Stop cleanly terminates the probe scheduler.
func (ps *ProbeScheduler) Stop() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.running.Swap(false) {
		if ps.stopCh != nil {
			close(ps.stopCh)
			ps.stopCh = nil
		}
	}
}

// evaluateAndSchedule scans all routes in store and schedules probes based on their tier interval.
func (ps *ProbeScheduler) evaluateAndSchedule(ctx context.Context, now time.Time) {
	cfg := ps.GetConfig()
	routes := ps.store.GetAll()

	for _, r := range routes {
		var interval time.Duration
		switch r.Tier {
		case TierActive:
			interval = cfg.ActiveInterval
		case TierStandby:
			interval = cfg.StandbyInterval
		case TierFailed:
			interval = cfg.FailedInterval
		default: // Candidate
			interval = cfg.CandidateInterval
		}

		if interval <= 0 {
			continue
		}

		if r.LastTested.IsZero() || now.Sub(r.LastTested) >= interval {
			// In-flight check: skip if probe is already running on this IP
			if _, inFlight := ps.inFlight.Load(r.IP); inFlight {
				continue
			}

			// Acquire concurrency slot
			select {
			case ps.probeSem <- struct{}{}:
				go func(routeIP string) {
					defer func() { <-ps.probeSem }()
					ps.ProbeOnce(ctx, routeIP, false)
				}(r.IP)
			default:
				// Work queue full, defer to next tick
			}
		}
	}

	// Periodically evaluate tier promotions and demotions across all managed routes
	ps.store.EvaluateTierTransitions(now)
}

// TriggerOnDemand queues an on-demand probe request.
func (ps *ProbeScheduler) TriggerOnDemand(ip string) {
	// Skip queuing if this IP is already being probed
	if _, inFlight := ps.inFlight.Load(ip); inFlight {
		return
	}
	select {
	case ps.probeTrigger <- ip:
	default:
	}
}
