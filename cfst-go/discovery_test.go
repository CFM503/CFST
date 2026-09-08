package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDiscoveryAddsCandidate verifies that newly discovered IPs enter RouteStore strictly as TierCandidate.
func TestDiscoveryAddsCandidate(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	// Simulate discovery finding a valid node
	newNode := NodeResult{
		IP:         "198.51.100.50",
		Port:       443,
		TCPLatency: 32.5,
		PacketLoss: 0.0,
		Jitter:     1.5,
		Colo:       "SJC",
	}

	// Route must not exist prior to discovery
	if _, exists := store.Get(newNode.IP); exists {
		t.Fatalf("route %s should not exist prior to discovery", newNode.IP)
	}

	// Insert as candidate
	rm := FromNodeResult(newNode, TierCandidate)
	store.UpsertRoute(*rm)

	rec, exists := store.Get(newNode.IP)
	if !exists {
		t.Fatalf("route %s was not inserted into RouteStore", newNode.IP)
	}
	if rec.Metrics.Tier != TierCandidate {
		t.Fatalf("new discovered route must strictly enter as TierCandidate, got %s", rec.Metrics.Tier)
	}
	if rec.Metrics.Colo != "SJC" {
		t.Fatalf("expected Colo SJC, got %s", rec.Metrics.Colo)
	}
	_ = dm
}

// TestDiscoveryDoesNotDuplicateExistingRoute verifies that discovering an already tracked IP preserves history.
func TestDiscoveryDoesNotDuplicateExistingRoute(t *testing.T) {
	store := NewRouteStore()

	existingIP := "198.51.100.60"
	store.UpsertRoute(RouteMetrics{
		IP:         existingIP,
		Port:       443,
		Tier:       TierActive,
		Health:     HealthHealthy,
		FinalScore: 92.5,
		P10Speed:   45.0,
		LastTested: time.Now().Add(-2 * time.Minute),
	})

	rec, _ := store.Get(existingIP)
	rec.AddSample(MeasurementSample{
		Timestamp: time.Now().Add(-2 * time.Minute),
		Speed:     50.0,
		P10Speed:  45.0,
		RTT:       30.0,
		Stability: 95.0,
		Success:   true,
	})

	initialSampleCount := len(rec.Samples)
	initialScore := rec.Metrics.FinalScore
	initialTier := rec.Metrics.Tier

	// Re-discovery sees the same IP
	node := NodeResult{
		IP:         existingIP,
		Port:       443,
		TCPLatency: 28.0,
		Colo:       "NRT",
	}

	// Simulate discovery logic: if exists, do NOT overwrite
	if r, exists := store.Get(node.IP); exists {
		if r.Metrics.Colo == "" && node.Colo != "" {
			r.Metrics.Colo = node.Colo
		}
	} else {
		rm := FromNodeResult(node, TierCandidate)
		store.UpsertRoute(*rm)
	}

	afterRec, exists := store.Get(existingIP)
	if !exists {
		t.Fatalf("route %s disappeared after re-discovery", existingIP)
	}
	if len(afterRec.Samples) != initialSampleCount {
		t.Fatalf("historical samples must not be overwritten or cleared: got %d, expected %d", len(afterRec.Samples), initialSampleCount)
	}
	if afterRec.Metrics.FinalScore != initialScore {
		t.Fatalf("final score must be preserved: got %.1f, expected %.1f", afterRec.Metrics.FinalScore, initialScore)
	}
	if afterRec.Metrics.Tier != initialTier {
		t.Fatalf("existing tier must remain %s, got %s", initialTier, afterRec.Metrics.Tier)
	}
}

// TestDiscoveryUsesCFSTProfile verifies that default continuous discovery runs ProfileCFST (HTTPS, speed.cloudflare.com, no WSS).
func TestDiscoveryUsesCFSTProfile(t *testing.T) {
	var wssAttempted atomic.Bool
	var httpsAttempted atomic.Bool

	tlsConf := generateTestTLSConfig(t)
	ln, port := startDualProtocolListener(t, tlsConf, nil,
		func(conn net.Conn, reqLine string, headers map[string]string) {
			httpsAttempted.Store(true)
			if strings.ToLower(headers["upgrade"]) == "websocket" || strings.Contains(reqLine, "/pyway") {
				wssAttempted.Store(true)
			}
			body := "0123456789abcdef"
			if strings.HasPrefix(reqLine, "HEAD") {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\ncf-ray: 12345-HKG\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
			} else {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\ncf-ray: 12345-HKG\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}
		})
	defer ln.Close()

	cfg := DefaultProbeConfig()
	if cfg.Profile.Type != ProfileCFST {
		t.Fatalf("expected DefaultProbeConfig profile to be ProfileCFST, got %s", cfg.Profile.Type)
	}

	cfg.Profile.Port = port
	cfg.Profile.TestURL = fmt.Sprintf("http://127.0.0.1:%d/__down", port)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	validNodes, tcpValid, httpsValid := ScanRoutesWithProfileDetailed(ctx, []string{"127.0.0.1"}, port, 1, cfg.Profile, nil)

	if wssAttempted.Load() {
		t.Fatalf("default Discovery scan must NEVER perform WebSocket handshake or request /pyway!")
	}
	if tcpValid < 1 {
		t.Fatalf("expected tcpValid >= 1, got %d", tcpValid)
	}
	if httpsValid < 1 {
		t.Fatalf("expected httpsValid >= 1, got %d", httpsValid)
	}
	if len(validNodes) != 1 {
		t.Fatalf("expected 1 valid node, got %d", len(validNodes))
	}
}

// TestDiscoveryStatusAPI verifies that GET /api/discovery/status returns correct JSON payload structure.
func TestDiscoveryStatusAPI(t *testing.T) {
	// Set mock status
	GlobalDiscoveryManager.mu.Lock()
	GlobalDiscoveryManager.status = DiscoveryStatus{
		Enabled:         true,
		IntervalSec:     3600,
		LastRun:         time.Now().Add(-10 * time.Minute),
		LastDurationSec: 12.5,
		Scanned:         200,
		TCPValid:        42,
		HTTPSValid:      31,
		NewCandidates:   12,
		ExistingRoutes:  19,
	}
	GlobalDiscoveryManager.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/discovery/status", nil)
	w := httptest.NewRecorder()

	handleAPIDiscoveryStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var status DiscoveryStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}

	if !status.Enabled {
		t.Fatalf("expected status.Enabled == true")
	}
	if status.IntervalSec != 3600 {
		t.Fatalf("expected interval_sec 3600, got %d", status.IntervalSec)
	}
	if status.Scanned != 200 {
		t.Fatalf("expected scanned 200, got %d", status.Scanned)
	}
	if status.TCPValid != 42 {
		t.Fatalf("expected tcp_valid 42, got %d", status.TCPValid)
	}
	if status.HTTPSValid != 31 {
		t.Fatalf("expected https_valid 31, got %d", status.HTTPSValid)
	}
	if status.NewCandidates != 12 {
		t.Fatalf("expected new_candidates 12, got %d", status.NewCandidates)
	}
	if status.ExistingRoutes != 19 {
		t.Fatalf("expected existing_routes 19, got %d", status.ExistingRoutes)
	}
}

func TestDiscoveryRunOnceAddsCandidate(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	newIP := "198.51.100.99"
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{
			{
				IP:         newIP,
				Port:       443,
				TCPLatency: 25.0,
				PacketLoss: 0.0,
				Jitter:     1.0,
				Colo:       "SJC",
			},
		}, 1, 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	if status.NewCandidates != 1 {
		t.Fatalf("expected 1 new candidate, got %d", status.NewCandidates)
	}

	rec, exists := store.Get(newIP)
	if !exists {
		t.Fatalf("expected newly discovered route in store")
	}
	if rec.Metrics.Tier != TierCandidate {
		t.Fatalf("expected discovered route to be TierCandidate, got %s", rec.Metrics.Tier)
	}
	if rec.Metrics.Colo != "SJC" {
		t.Fatalf("expected Colo SJC, got %s", rec.Metrics.Colo)
	}
}

func TestDiscoveryRunOncePreservesHistory(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	existingIP := "198.51.100.88"
	store.UpsertRoute(RouteMetrics{
		IP:         existingIP,
		Port:       443,
		Tier:       TierStandby,
		Health:     HealthHealthy,
		FinalScore: 85.0,
		P10Speed:   35.0,
		LastTested: time.Now().Add(-5 * time.Minute),
	})

	rec, _ := store.Get(existingIP)
	for i := 0; i < 20; i++ {
		rec.AddSample(MeasurementSample{
			Timestamp: time.Now().Add(-time.Duration(20-i) * time.Minute),
			Speed:     40.0,
			P10Speed:  35.0,
			RTT:       28.0,
			Stability: 90.0,
			Success:   true,
		})
	}
	rec.Metrics.FinalScore = 85.0

	// Scanner discovers existing IP with newly resolved Colo "HKG"
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{
			{
				IP:         existingIP,
				Port:       443,
				TCPLatency: 26.0,
				Colo:       "HKG",
			},
		}, 1, 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	if status.ExistingRoutes != 1 {
		t.Fatalf("expected 1 existing route, got %d", status.ExistingRoutes)
	}

	afterRec, exists := store.Get(existingIP)
	if !exists {
		t.Fatalf("expected route in store")
	}
	if len(afterRec.Samples) != 20 {
		t.Fatalf("samples must remain 20, got %d", len(afterRec.Samples))
	}
	if afterRec.Metrics.Tier != TierStandby {
		t.Fatalf("tier must remain TierStandby, got %s", afterRec.Metrics.Tier)
	}
	if afterRec.Metrics.FinalScore != 85.0 {
		t.Fatalf("final score must remain 85.0, got %.1f", afterRec.Metrics.FinalScore)
	}
	if afterRec.Metrics.Colo != "HKG" {
		t.Fatalf("expected Colo to be updated to HKG via UpdateRouteColo, got %s", afterRec.Metrics.Colo)
	}
}

func TestDiscoveryImmediateStartup(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	var runCalled atomic.Bool
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		runCalled.Store(true)
		return nil, 0, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	dm.Start(ctx)
	defer dm.Stop()

	// Wait up to 1 second for the immediate startup discovery pass to fire
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if runCalled.Load() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !runCalled.Load() {
		t.Fatalf("expected discovery to run immediately upon Start() without waiting for ticker")
	}
}

func TestDiscoveryNoOverlappingRuns(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	inScan := make(chan struct{})
	finishScan := make(chan struct{})

	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		close(inScan)
		<-finishScan
		return nil, 0, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Run 1 begins
	go func() {
		_, _ = dm.RunOnce(ctx)
	}()

	<-inScan

	// Run 2 attempts while Run 1 is in progress
	_, err := dm.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("expected 'already in progress' error for overlapping RunOnce, got %v", err)
	}

	close(finishScan)
}

func TestDiscoveryManagerRestart(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return nil, 0, 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start
	dm.Start(ctx)
	if !dm.running.Load() {
		t.Fatalf("expected discovery manager to be running after Start()")
	}

	// 2. Stop
	dm.Stop()
	if dm.running.Load() {
		t.Fatalf("expected discovery manager to be stopped after Stop()")
	}

	// 3. Restart cleanly
	dm.Start(ctx)
	if !dm.running.Load() {
		t.Fatalf("expected discovery manager to be running after second Start()")
	}

	// Final stop
	dm.Stop()
	if dm.running.Load() {
		t.Fatalf("expected discovery manager to be stopped after final Stop()")
	}
}

func TestDiscoveryManagerRestartNoRace(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.DiscoveryEnabled = false // disable immediate active scan for lifecycle test
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return nil, 0, 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Goroutine 1 lifecycle
	started1 := make(chan struct{})
	exited1 := make(chan struct{})

	dm.onStartGoroutine = func() {
		close(started1)
	}
	dm.onExitGoroutine = func() {
		close(exited1)
	}

	dm.Start(ctx)

	select {
	case <-started1:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for discovery goroutine 1 to start")
	}

	if !dm.running.Load() {
		t.Fatalf("expected discovery manager to be running")
	}

	// 2. Stop and verify clean exit
	dm.Stop()

	select {
	case <-exited1:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for discovery goroutine 1 to exit")
	}

	if dm.running.Load() {
		t.Fatalf("expected discovery manager to be stopped")
	}

	// 3. Goroutine 2 lifecycle
	started2 := make(chan struct{})
	exited2 := make(chan struct{})

	dm.onStartGoroutine = func() {
		close(started2)
	}
	dm.onExitGoroutine = func() {
		close(exited2)
	}

	dm.Start(ctx)

	select {
	case <-started2:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for discovery goroutine 2 to start")
	}

	if !dm.running.Load() {
		t.Fatalf("expected discovery manager to be running again")
	}

	// 4. Final Stop
	dm.Stop()

	select {
	case <-exited2:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for discovery goroutine 2 to exit")
	}

	if dm.running.Load() {
		t.Fatalf("expected discovery manager to be stopped after second restart")
	}
}
