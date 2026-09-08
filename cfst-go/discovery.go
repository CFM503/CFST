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
	mu      sync.RWMutex
	status  DiscoveryStatus
	store   *RouteStore
	sched   *ProbeScheduler
	running atomic.Bool
	stopCh  chan struct{}
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
	if dm.running.Swap(true) {
		return // already running
	}

	go func() {
		cfg := dm.sched.GetConfig()
		interval := cfg.DiscoveryInterval
		if interval <= 0 {
			interval = 60 * time.Minute
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-dm.stopCh:
				dm.running.Store(false)
				return
			case <-ctx.Done():
				dm.running.Store(false)
				return
			case <-ticker.C:
				cfg := dm.sched.GetConfig()
				if !cfg.DiscoveryEnabled {
					continue
				}
				_, _ = dm.RunOnce(ctx)
				// Re-align ticker if interval changed
				if cfg.DiscoveryInterval > 0 && cfg.DiscoveryInterval != interval {
					interval = cfg.DiscoveryInterval
					ticker.Reset(interval)
				}
			}
		}
	}()
}

// Stop cleanly halts the background discovery worker.
func (dm *DiscoveryManager) Stop() {
	if dm.running.Swap(false) {
		close(dm.stopCh)
	}
}

// RunOnce performs a single low-bandwidth discovery pass across Cloudflare IP space.
func (dm *DiscoveryManager) RunOnce(ctx context.Context) (DiscoveryStatus, error) {
	start := time.Now()
	cfg := dm.sched.GetConfig()

	scanCount := cfg.DiscoveryScanCount
	if scanCount <= 0 {
		scanCount = 200
	}

	ips := GenerateIPs(scanCount, true, "")
	if len(ips) == 0 {
		return dm.GetStatus(), fmt.Errorf("no Cloudflare IPs generated")
	}

	fmt.Printf("\n[Discovery]\nScanning %d Cloudflare IPs...\n", len(ips))

	scanConcurrency := 50
	if cfg.MaxConcurrent > 50 {
		scanConcurrency = cfg.MaxConcurrent
	}

	validNodes, tcpValid, httpsValid := ScanRoutesWithProfileDetailed(ctx, ips, cfg.Port, scanConcurrency, cfg.Profile, nil)

	newCandidates := 0
	existingRoutes := 0
	var bestNewCandidate *RouteMetrics

	for _, node := range validNodes {
		if rec, exists := dm.store.Get(node.IP); exists {
			// Existing route: preserve all historical samples, EWMA, and stability scores
			existingRoutes++
			if rec.Metrics.Colo == "" && node.Colo != "" {
				rec.Metrics.Colo = node.Colo
			}
		} else {
			// New route: strictly enter as TierCandidate
			newCandidates++
			rm := FromNodeResult(node, TierCandidate)
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
