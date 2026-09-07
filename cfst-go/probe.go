package main

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ProbeConfig holds parameters controlling tiered background probing.
type ProbeConfig struct {
	ActiveInterval    time.Duration `json:"active_interval"`    // High frequency for active routes
	StandbyInterval   time.Duration `json:"standby_interval"`   // Medium frequency for standby routes
	CandidateInterval time.Duration `json:"candidate_interval"` // Low frequency for candidate pool
	FailedInterval    time.Duration `json:"failed_interval"`    // Periodic recovery checks for failed routes
	MaxConcurrent     int           `json:"max_concurrent"`     // Concurrency limit for background ping/wss
	MaxSpeedTests     int           `json:"max_speed_tests"`    // Strict limit (usually 1) for concurrent speed tests
	QuickDuration     int           `json:"quick_duration"`     // Duration in seconds for L3 quick speed test
	FullDuration      int           `json:"full_duration"`      // Duration in seconds for L4 full speed test
	SpeedTestCycle    int           `json:"speed_test_cycle"`   // Perform speed test every N cycles on Active/Standby
	WSSHost           string        `json:"wss_host"`
	SNI               string        `json:"sni"`
	URL               string        `json:"url"`
	Port              int           `json:"port"`
}

func DefaultProbeConfig() ProbeConfig {
	return ProbeConfig{
		ActiveInterval:    10 * time.Second,
		StandbyInterval:   30 * time.Second,
		CandidateInterval: 180 * time.Second,
		FailedInterval:    60 * time.Second,
		MaxConcurrent:     4,
		MaxSpeedTests:     1,
		QuickDuration:     3,
		FullDuration:      10,
		SpeedTestCycle:    6, // every 6th cycle on Active/Standby (approx 1-3 mins)
		WSSHost:           "colo.4467107.xyz",
		SNI:               "",
		URL:               "https://speed.cloudflare.com/__down?bytes=500000000",
		Port:              443,
	}
}

// LayeredProbeResult contains metrics produced by a multi-layer probe pass.
type LayeredProbeResult struct {
	Success          bool
	RTT              float64
	PacketLoss       float64
	Jitter           float64
	HandshakeSuccess bool
	SingleSpeed      float64
	MinSpeed         float64
	Stability        float64
	LoadLatency      float64
	Colo             string
	Error            string
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

// ExecuteLayeredProbe executes L1 to L5 sequentially on an IP.
func (ps *ProbeScheduler) ExecuteLayeredProbe(ctx context.Context, ip string, port int, runSpeedTest bool, fullSpeed bool) LayeredProbeResult {
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
	// Layer 3 & Layer 4: Speed Tests (Bandwidth-throttled)
	// -------------------------------------------------------------
	if runSpeedTest {
		// Acquire speed semaphore to strictly prevent bandwidth congestion
		select {
		case ps.speedSem <- struct{}{}:
			defer func() { <-ps.speedSem }()
		case <-ctx.Done():
			res.Error = "Cancelled waiting for speed test slot"
			return res
		}

		duration := cfg.QuickDuration
		if fullSpeed {
			duration = cfg.FullDuration
		}
		if duration < 2 {
			duration = 2
		}

		speed, minSpd, stab := SingleStreamTest(ctx, ip, port, duration, cfg.URL, cfg.SNI, nil)
		res.SingleSpeed = speed
		res.MinSpeed = minSpd
		res.Stability = stab

		if speed <= 0 && minSpd <= 0 {
			res.Error = "Speed test returned 0 MB/s"
			return res
		}

		// Detect Colo if unknown
		res.Colo = GetColo(ip, port)

		// -------------------------------------------------------------
		// Layer 5: Load Latency (optional, only on full tests)
		// -------------------------------------------------------------
		if fullSpeed && !isCustomURL(cfg.URL) {
			res.LoadLatency = MeasureLoadLatency(ip, port)
		}
	}

	res.Success = true
	return res
}

// ProbeOnce executes a probe pass for a single route and updates the store.
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
	val, _ := ps.cycleCount.LoadOrStore(ip, int64(0))
	cycle := val.(int64) + 1
	ps.cycleCount.Store(ip, cycle)

	runSpeedTest := forceSpeed
	fullSpeed := false
	if !runSpeedTest {
		switch tier {
		case TierActive:
			// Active route: prioritize low-overhead L1/L2, periodically test speed
			if cycle%int64(cfg.SpeedTestCycle) == 0 {
				runSpeedTest = true
				fullSpeed = false // quick speed test on active route to avoid eating bandwidth
			}
		case TierStandby:
			if cycle%int64(cfg.SpeedTestCycle) == 0 {
				runSpeedTest = true
				fullSpeed = true // standby route can test full speed
			}
		case TierCandidate:
			// Candidates test speed occasionally
			if cycle%int64(cfg.SpeedTestCycle*2) == 0 {
				runSpeedTest = true
				fullSpeed = false
			}
		case TierFailed:
			// Failed routes only do L1/L2 recovery checks, no speed test
			runSpeedTest = false
		}
	}

	result := ps.ExecuteLayeredProbe(ctx, ip, port, runSpeedTest, fullSpeed)

	now := time.Now()
	m := RouteMetrics{
		ID:                 GenerateRouteID(ip, port),
		IP:                 ip,
		Port:               port,
		Colo:               result.Colo,
		Tier:               tier,
		RTT:                result.RTT,
		PacketLoss:         result.PacketLoss,
		Jitter:             result.Jitter,
		HandshakeSuccess:   result.HandshakeSuccess,
		DownloadSpeed:      result.SingleSpeed,
		SingleSpeed:        result.SingleSpeed,
		MinSpeed:           result.MinSpeed,
		Stability:          result.Stability,
		LoadLatency:        result.LoadLatency,
		LastTested:         now,
		Timestamp:          now,
		ConsecutiveFails:   0,
		ConsecutiveSuccess: 0,
	}

	if m.Colo == "" {
		m.Colo = existingColo
	}
	if exists {
		m.ConsecutiveFails = rec.Metrics.ConsecutiveFails
		m.ConsecutiveSuccess = rec.Metrics.ConsecutiveSuccess
		m.Health = rec.Metrics.Health
		// If speed test wasn't run on this cycle, inherit previous speeds
		if !runSpeedTest {
			m.DownloadSpeed = rec.Metrics.DownloadSpeed
			m.SingleSpeed = rec.Metrics.SingleSpeed
			m.MinSpeed = rec.Metrics.MinSpeed
			m.Stability = rec.Metrics.Stability
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
