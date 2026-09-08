package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// DiscoveryStatus exposes the runtime state and results of the continuous discovery engine.
type DiscoveryStatus struct {
	Enabled         bool      `json:"enabled"`
	IntervalSec     int       `json:"interval_sec"`
	LastRun         time.Time `json:"last_run"`
	LastDurationSec float64   `json:"last_duration_sec"`
	Scanned         int       `json:"scanned"`
	TCPValid        int       `json:"tcp_valid"`
	HTTPSValid      int       `json:"https_valid"`
	NewCandidates   int       `json:"new_candidates"`
	ExistingRoutes  int       `json:"existing_routes"`
}

// DiscoveryManager orchestrates periodic background discovery of Cloudflare edge IPs.
type DiscoveryManager struct {
	mu               sync.RWMutex
	status           DiscoveryStatus
	store            *RouteStore
	sched            *ProbeScheduler
	running          atomic.Bool
	inProgress       atomic.Bool
	stopCh           chan struct{}
	scanFunc         func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int)
	onStartGoroutine func()
	onExitGoroutine  func()
}

var GlobalDiscoveryManager = NewDiscoveryManager(GlobalRouteStore, GlobalProbeScheduler)

func NewDiscoveryManager(store *RouteStore, sched *ProbeScheduler) *DiscoveryManager {
	return &DiscoveryManager{
		store:  store,
		sched:  sched,
		stopCh: make(chan struct{}),
		status: DiscoveryStatus{
			Enabled:     true,
			IntervalSec: 3600,
		},
	}
}

// GetStatus returns a snapshot of current discovery status.
func (dm *DiscoveryManager) GetStatus() DiscoveryStatus {
	dm.mu.RLock()
	defer dm.mu.RUnlock()
	st := dm.status
	cfg := dm.sched.GetConfig()
	st.Enabled = cfg.DiscoveryEnabled
	st.IntervalSec = int(cfg.DiscoveryInterval.Seconds())
	return st
}

// Start launches the continuous background discovery worker.
func (dm *DiscoveryManager) Start(ctx context.Context) {
	dm.mu.Lock()
	if dm.running.Load() {
		dm.mu.Unlock()
		return // already running
	}
	dm.running.Store(true)
	stopCh := make(chan struct{})
	dm.stopCh = stopCh
	dm.mu.Unlock()

	// 1. Trigger immediate initial discovery pass asynchronously on startup
	go func(myCtx context.Context) {
		if myCtx.Err() != nil {
			return
		}
		cfg := dm.sched.GetConfig()
		if cfg.DiscoveryEnabled {
			_, _ = dm.RunOnce(myCtx)
		}
	}(ctx)

	// 2. Periodic background discovery loop
	go func(myStopCh chan struct{}, myCtx context.Context) {
		if dm.onStartGoroutine != nil {
			dm.onStartGoroutine()
		}
		defer func() {
			if dm.onExitGoroutine != nil {
				dm.onExitGoroutine()
			}
		}()

		cfg := dm.sched.GetConfig()
		interval := cfg.DiscoveryInterval
		if interval <= 0 {
			interval = 60 * time.Minute
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-myStopCh:
				return
			case <-myCtx.Done():
				// Only clear running state if this generation is still current.
				// Prevents a stale ctx-cancel from clobbering a newer Start().
				dm.mu.Lock()
				if dm.stopCh == myStopCh {
					dm.running.Store(false)
					dm.stopCh = nil
				}
				dm.mu.Unlock()
				return
			case <-ticker.C:
				cfg := dm.sched.GetConfig()
				if !cfg.DiscoveryEnabled {
					continue
				}
				_, _ = dm.RunOnce(myCtx)
				// Re-align ticker if interval changed
				if cfg.DiscoveryInterval > 0 && cfg.DiscoveryInterval != interval {
					interval = cfg.DiscoveryInterval
					ticker.Reset(interval)
				}
			}
		}
	}(stopCh, ctx)
}

// Stop cleanly halts the background discovery worker.
func (dm *DiscoveryManager) Stop() {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.running.Swap(false) {
		if dm.stopCh != nil {
			close(dm.stopCh)
			dm.stopCh = nil
		}
	}
}

// RunOnce performs a single low-bandwidth discovery pass across Cloudflare IP space.
func (dm *DiscoveryManager) RunOnce(ctx context.Context) (DiscoveryStatus, error) {
	if !dm.inProgress.CompareAndSwap(false, true) {
		return dm.GetStatus(), fmt.Errorf("discovery already in progress")
	}
	defer dm.inProgress.Store(false)

	start := time.Now()
	cfg := dm.sched.GetConfig()

	scanCount := cfg.DiscoveryScanCount
	if scanCount <= 0 {
		scanCount = 200
	}

	ips := GenerateIPs(scanCount, true, "")
	if len(ips) == 0 {
		return dm.GetStatus(), fmt.Errorf("no candidate IPs generated for discovery")
	}

	fmt.Printf("\n[Discovery]\nScanning %d Cloudflare IPs...\n", len(ips))

	scanConcurrency := 50
	if cfg.MaxConcurrent > 50 {
		scanConcurrency = cfg.MaxConcurrent
	}

	scanFn := dm.scanFunc
	if scanFn == nil {
		scanFn = ScanRoutesWithProfileDetailed
	}

	validNodes, tcpValid, httpsValid := scanFn(ctx, ips, cfg.Port, scanConcurrency, cfg.Profile, nil)

	newCandidates := 0
	existingRoutes := 0
	var bestNewCandidate *RouteMetrics

	isGOWAYWSS := cfg.Profile.Type == ProfileGOWAYWSS || (cfg.Profile.Type == ProfileCustom && cfg.Profile.Protocol == "wss")

	for _, node := range validNodes {
		// Strict GOWAY-WSS compatibility gate:
		// When profile is GOWAY-WSS, ONLY nodes with GOWAYWSSCompatible == true can enter or remain as valid candidates.
		// Nodes returning HTTP 200, 403, 404, 502 or with failed WSS handshakes are strictly rejected.
		if isGOWAYWSS && !node.GOWAYWSSCompatible {
			if rec, exists := dm.store.Get(node.IP); exists {
				// Existing route failed WSS check in this round: record failure so it cannot bypass as valid candidate
				rm := rec.Metrics
				rm.GOWAYWSSCompatible = false
				rm.HandshakeSuccess = false
				rm.GOWAYWSSLatency = node.GOWAYWSSLatency
				rm.GOWAYWSSErrorStage = node.GOWAYWSSErrorStage
				rm.GOWAYWSSHTTPStatus = node.GOWAYWSSHTTPStatus
				rm.GOWAYWSSErrorMessage = node.GOWAYWSSErrorMessage
				rm.GOWAYWSSSNISent = node.GOWAYWSSSNISent
				rm.GOWAYWSSHostSent = node.GOWAYWSSHostSent
				rm.GOWAYWSSPathSent = node.GOWAYWSSPathSent
				dm.store.RecordProbeResult(rm, false)
			}
			continue
		}

		if rec, exists := dm.store.Get(node.IP); exists {
			// Existing route: preserve all historical samples, EWMA, and stability scores
			existingRoutes++
			if node.Colo != "" {
				dm.store.UpdateRouteColo(node.IP, node.Colo)
			}
			if isGOWAYWSS {
				rm := rec.Metrics
				rm.GOWAYWSSCompatible = true
				rm.HandshakeSuccess = true
				rm.GOWAYWSSLatency = node.GOWAYWSSLatency
				rm.GOWAYWSSErrorStage = node.GOWAYWSSErrorStage
				rm.GOWAYWSSHTTPStatus = node.GOWAYWSSHTTPStatus
				rm.GOWAYWSSErrorMessage = node.GOWAYWSSErrorMessage
				rm.GOWAYWSSSNISent = node.GOWAYWSSSNISent
				rm.GOWAYWSSHostSent = node.GOWAYWSSHostSent
				rm.GOWAYWSSPathSent = node.GOWAYWSSPathSent
				dm.store.UpsertRoute(rm)
			}
		} else {
			// New route: strictly enter as TierCandidate
			newCandidates++
			rm := FromNodeResult(node, TierCandidate)
			if isGOWAYWSS {
				rm.GOWAYWSSCompatible = true
				rm.HandshakeSuccess = true
				rm.GOWAYWSSLatency = node.GOWAYWSSLatency
				rm.GOWAYWSSErrorStage = node.GOWAYWSSErrorStage
				rm.GOWAYWSSHTTPStatus = node.GOWAYWSSHTTPStatus
				rm.GOWAYWSSErrorMessage = node.GOWAYWSSErrorMessage
				rm.GOWAYWSSSNISent = node.GOWAYWSSSNISent
				rm.GOWAYWSSHostSent = node.GOWAYWSSHostSent
				rm.GOWAYWSSPathSent = node.GOWAYWSSPathSent
			} else {
				rm.GOWAYWSSCompatible = false
			}
			dm.store.UpsertRoute(*rm)

			if bestNewCandidate == nil || rm.FinalScore > bestNewCandidate.FinalScore || (rm.FinalScore == bestNewCandidate.FinalScore && rm.RTT < bestNewCandidate.RTT) {
				bestNewCandidate = rm
			}
		}
	}

	durationSec := math.Round(time.Since(start).Seconds()*10) / 10

	dm.mu.Lock()
	dm.status = DiscoveryStatus{
		Enabled:         cfg.DiscoveryEnabled,
		IntervalSec:     int(cfg.DiscoveryInterval.Seconds()),
		LastRun:         start,
		LastDurationSec: durationSec,
		Scanned:         len(ips),
		TCPValid:        tcpValid,
		HTTPSValid:      httpsValid,
		NewCandidates:   newCandidates,
		ExistingRoutes:  existingRoutes,
	}
	resStatus := dm.status
	dm.mu.Unlock()

	fmt.Println("[Discovery]")
	fmt.Printf("TCP valid: %d\n", tcpValid)
	fmt.Printf("HTTPS valid: %d\n", httpsValid)
	fmt.Printf("New candidates: %d\n", newCandidates)
	fmt.Printf("Existing routes: %d\n", existingRoutes)

	if bestNewCandidate != nil {
		fmt.Println("[Discovery]")
		fmt.Println("Best candidate:")
		fmt.Println(bestNewCandidate.IP)
		fmt.Printf("Score: %.1f\n", bestNewCandidate.FinalScore)
		fmt.Printf("P10: %.1f MB/s\n", bestNewCandidate.P10Speed)
		fmt.Printf("Stability: %.1f\n", bestNewCandidate.Stability)
	}

	return resStatus, nil
}
