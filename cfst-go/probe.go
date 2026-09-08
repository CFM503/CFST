package main

import (
	"context"
	"math"
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
	if host == "" {
		host = "colo.4467107.xyz"
	}
	if path == "" {
		path = "/pyway"
	}
	if sni == "" {
		sni = host
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
	if port <= 0 {
		port = 443
	}
	return ProbeProfile{
		Type:     ProfileCustom,
		Port:     port,
		SNI:      sni,
		Host:     sni,
		Path:     "/",
		TestURL:  customURL,
		Protocol: "https",
	}
}

// ProbeConfig holds parameters controlling tiered background probing.
type ProbeConfig struct {
	Profile           ProbeProfile  `json:"profile"`
	ActiveInterval    time.Duration `json:"active_interval"`    // High frequency for active routes
	StandbyInterval   time.Duration `json:"standby_interval"`   // Medium frequency for standby routes
	CandidateInterval time.Duration `json:"candidate_interval"` // Low frequency for candidate pool
	FailedInterval    time.Duration `json:"failed_interval"`    // Periodic recovery checks for failed routes
	MaxConcurrent     int           `json:"max_concurrent"`     // Concurrency limit for background ping/wss
	MaxSpeedTests     int           `json:"max_speed_tests"`    // Strict limit (usually 1) for concurrent speed tests
	QuickDuration     int           `json:"quick_duration"`     // Duration in seconds for L3 quick speed test
	FullDuration      int           `json:"full_duration"`      // Duration in seconds for L4 full speed test
	L3ProbeCycle      int           `json:"l3_probe_cycle"`     // Lightweight HTTP check every N cycles (e.g. 3)
	FullSpeedCycle    int           `json:"full_speed_cycle"`   // Full speed test calibration cycle (e.g. 60)
	SpeedTestCycle    int           `json:"speed_test_cycle"`   // Legacy alias for FullSpeedCycle
	WSSHost           string        `json:"wss_host"`
	SNI               string        `json:"sni"`
	URL               string        `json:"url"`
	Port              int           `json:"port"`
}

func DefaultProbeConfig() ProbeConfig {
	prof := NewProfileCFST()
	return ProbeConfig{
		Profile:           prof,
		ActiveInterval:    10 * time.Second,
		StandbyInterval:   30 * time.Second,
		CandidateInterval: 180 * time.Second,
		FailedInterval:    60 * time.Second,
		MaxConcurrent:     4,
		MaxSpeedTests:     1,
		QuickDuration:     3,
		FullDuration:      10,
		L3ProbeCycle:      3,  // L3 lightweight 100KB probe every 3 cycles (~30s on Active)
		FullSpeedCycle:    60, // L4 full speed calibration every 60 cycles (~10m on Active)
		SpeedTestCycle:    60,
		WSSHost:           "colo.4467107.xyz",
		SNI:               prof.SNI,
		URL:               prof.TestURL,
		Port:              prof.Port,
	}
}

// LayeredProbeResult contains metrics produced by a multi-layer probe pass.
type LayeredProbeResult struct {
	Success              bool
	RTT                  float64
	PacketLoss           float64
	Jitter               float64
	HandshakeSuccess     bool
	SingleSpeed          float64
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
	mu           sync.RWMutex
	store        *RouteStore
	cfg          ProbeConfig
	speedSem     chan struct{} // limits concurrent bandwidth-heavy speed tests
	probeSem     chan struct{} // limits lightweight concurrent pings/handshakes
	cycleCount   sync.Map      // route IP -> int64 cycle count
	running      atomic.Bool
	stopCh       chan struct{}
	probeTrigger chan string
}

var GlobalProbeScheduler = NewProbeScheduler(GlobalRouteStore, DefaultProbeConfig())

func NewProbeScheduler(store *RouteStore, cfg ProbeConfig) *ProbeScheduler {
	if cfg.MaxSpeedTests < 1 {
		cfg.MaxSpeedTests = 1
	}
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 2
	}
	if cfg.L3ProbeCycle < 1 {
		cfg.L3ProbeCycle = 3
	}
	if cfg.FullSpeedCycle < 1 {
		cfg.FullSpeedCycle = 60
	}
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
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.cfg = cfg
}

func (ps *ProbeScheduler) GetConfig() ProbeConfig {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.cfg
}

// ExecuteLayeredProbe executes L1 to L5 sequentially on an IP, strictly minimizing bandwidth usage.
func (ps *ProbeScheduler) ExecuteLayeredProbe(ctx context.Context, ip string, port int, runL3 bool, runFullSpeed bool) LayeredProbeResult {
	cfg := ps.GetConfig()
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
		lat := TCPPing(ip, port, 1500*time.Millisecond)
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
	// Layer 2: WSS Handshake (Simulates goway TLS WebSocket upgrade)
	// -------------------------------------------------------------
	if cfg.WSSHost != "" {
		sni := cfg.SNI
		if sni == "" {
			sni = cfg.WSSHost
		}
		if !WSSHandshakeCheck(ip, port, sni, 3*time.Second) {
			res.HandshakeSuccess = false
			res.Error = "WSS handshake failed (e.g. 403 or TLS error)"
			return res
		}
		res.HandshakeSuccess = true
	} else {
		res.HandshakeSuccess = true
	}

	// -------------------------------------------------------------
	// Layer 3: Lightweight HTTP / HTTPS Probe (~100KB payload)
	// -------------------------------------------------------------
	if runL3 && !runFullSpeed {
		l3Res := LightweightHTTPProbe(ctx, ip, port, cfg.URL, cfg.SNI)
		if !l3Res.Success {
			res.Error = "L3 HTTP probe failed: " + l3Res.Error
			return res
		}
		if l3Res.Colo != "" {
			res.Colo = l3Res.Colo
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

		sm := SingleStreamTestDetailed(ctx, ip, port, duration, cfg.URL, cfg.SNI, nil, res.RTT, res.Jitter, res.PacketLoss)
		res.SingleSpeed = sm.AverageSpeed
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

		// Detect Colo if unknown
		if res.Colo == "" {
			res.Colo = GetColo(ip, port)
		}

		// Layer 5: Load Latency (optional)
		if !isCustomURL(cfg.URL) {
			res.LoadLatency = MeasureLoadLatency(ip, port)
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
	rec, exists := ps.store.Get(ip)
	port := ps.cfg.Port
	tier := TierCandidate
	existingColo := ""
	if exists {
		port = rec.Metrics.Port
		tier = rec.Metrics.Tier
		existingColo = rec.Metrics.Colo
	}

	// Determine if speed test is scheduled
	cfg := ps.GetConfig()
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
			if cycle%int64(l3Cycle) == 0 {
				runL3 = true
			}
			// L4 full speed calibration only every FullSpeedCycle (default 60 cycles = ~10m)
			if cycle%int64(fullCycle) == 0 {
				runFullSpeed = true
			}
		case TierStandby:
			// Standby route: lower frequency
			if cycle%int64(l3Cycle*2) == 0 {
				runL3 = true
			}
			if cycle%int64(fullCycle*2) == 0 {
				runFullSpeed = true
			}
		case TierCandidate:
			// Candidate: low frequency
			if cycle%int64(l3Cycle*4) == 0 {
				runL3 = true
			}
			runFullSpeed = false
		case TierFailed:
			// Failed routes: ONLY L1/L2 recovery checks, ZERO speed tests!
			runL3 = false
			runFullSpeed = false
		}
	}

	result := ps.ExecuteLayeredProbe(ctx, ip, port, runL3, runFullSpeed)

	now := time.Now()
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
		m.ConsecutiveFails = rec.Metrics.ConsecutiveFails
		m.ConsecutiveSuccess = rec.Metrics.ConsecutiveSuccess
		m.ConsecutiveDegraded = rec.Metrics.ConsecutiveDegraded
		m.Health = rec.Metrics.Health
		// If full speed test wasn't run on this cycle, inherit previous speeds
		if !runFullSpeed {
			m.DownloadSpeed = rec.Metrics.DownloadSpeed
			m.SingleSpeed = rec.Metrics.SingleSpeed
			m.P10Speed = rec.Metrics.P10Speed
			m.MedianSpeed = rec.Metrics.MedianSpeed
			m.MinSpeed = rec.Metrics.MinSpeed
			m.Stability = rec.Metrics.Stability
			m.CV = rec.Metrics.CV
			m.StallCount = rec.Metrics.StallCount
			m.ZeroSpeedIntervals = rec.Metrics.ZeroSpeedIntervals
			m.TotalStallDuration = rec.Metrics.TotalStallDuration
			m.LongestStallDuration = rec.Metrics.LongestStallDuration
			m.StallRate = rec.Metrics.StallRate
			m.LoadLatency = rec.Metrics.LoadLatency
		}
	}

	ps.store.RecordProbeResult(m, result.Success)
	updated, _ := ps.store.Get(ip)
	if updated != nil {
		return &updated.Metrics, nil
	}
	return &m, nil
}

// Start launches the background probing daemon.
func (ps *ProbeScheduler) Start(ctx context.Context) {
	if ps.running.Swap(true) {
		return // already running
	}

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ps.stopCh:
				ps.running.Store(false)
				return
			case <-ctx.Done():
				ps.running.Store(false)
				return
			case ip := <-ps.probeTrigger:
				go func(targetIP string) {
					ps.ProbeOnce(ctx, targetIP, true)
				}(ip)
			case now := <-ticker.C:
				ps.evaluateAndSchedule(ctx, now)
			}
		}
	}()
}

// Stop cleanly terminates the probe scheduler.
func (ps *ProbeScheduler) Stop() {
	if ps.running.Swap(false) {
		close(ps.stopCh)
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
}

// TriggerOnDemand queues an on-demand probe request.
func (ps *ProbeScheduler) TriggerOnDemand(ip string) {
	select {
	case ps.probeTrigger <- ip:
	default:
	}
}
