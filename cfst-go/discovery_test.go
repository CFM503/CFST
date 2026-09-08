package main

import (
	"bufio"
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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "closed") {
					return
				}
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				br := bufio.NewReader(c)
				reqLine, err := br.ReadString('\n')
				if err != nil {
					return // plain-TCP L1 ping: connected, sent nothing
				}
				fields := strings.Split(strings.TrimSpace(reqLine), " ")
				method, reqPath := "", "/"
				if len(fields) >= 2 {
					method = strings.ToUpper(fields[0])
					reqPath = fields[1]
				}
				upgrade := ""
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(line) == "" {
						break
					}
					if idx := strings.Index(line, ":"); idx > 0 {
						if strings.ToLower(strings.TrimSpace(line[:idx])) == "upgrade" {
							upgrade = strings.TrimSpace(line[idx+1:])
						}
					}
				}
				if strings.ToLower(upgrade) == "websocket" || reqPath == "/pyway" {
					wssAttempted.Store(true)
				}
				httpsAttempted.Store(true)
				body := "0123456789abcdef"
				if method == "HEAD" {
					_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\ncf-ray: 12345-HKG\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
				} else {
					_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\ncf-ray: 12345-HKG\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
				}
			}(conn)
		}
	}()

	// Readiness pre-flight
	for i := 0; i < 20; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

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

// --- GOWAY-WSS Discovery Admission Gate Tests ---

func TestDiscoveryGOWAYWSS101EntersCandidate(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.101"
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{
			{
				IP:                 targetIP,
				Port:               443,
				TCPLatency:         25.0,
				Colo:               "HKG",
				GOWAYWSSCompatible: true,
				GOWAYWSSLatency:    26.5,
				GOWAYWSSHTTPStatus: 101,
				GOWAYWSSSNISent:    "sni.example.com",
				GOWAYWSSHostSent:   "wss.example.com",
				GOWAYWSSPathSent:   "/pyway",
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

	rec, exists := store.Get(targetIP)
	if !exists {
		t.Fatalf("route %s not inserted into RouteStore", targetIP)
	}
	if rec.Metrics.Tier != TierCandidate {
		t.Fatalf("expected TierCandidate, got %s", rec.Metrics.Tier)
	}
	if !rec.Metrics.GOWAYWSSCompatible {
		t.Fatalf("expected GOWAYWSSCompatible=true")
	}
	if rec.Metrics.GOWAYWSSHTTPStatus != 101 {
		t.Fatalf("expected HTTP status 101, got %d", rec.Metrics.GOWAYWSSHTTPStatus)
	}
	if rec.Metrics.GOWAYWSSHostSent != "wss.example.com" {
		t.Fatalf("expected Host wss.example.com, got %s", rec.Metrics.GOWAYWSSHostSent)
	}
	if rec.Metrics.GOWAYWSSSNISent != "sni.example.com" {
		t.Fatalf("expected SNI sni.example.com, got %s", rec.Metrics.GOWAYWSSSNISent)
	}
	if rec.Metrics.GOWAYWSSPathSent != "/pyway" {
		t.Fatalf("expected Path /pyway, got %s", rec.Metrics.GOWAYWSSPathSent)
	}
}

func TestDiscoveryGOWAYWSS403Blocked(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.102"
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{
			{
				IP:                   targetIP,
				Port:                 443,
				TCPLatency:           25.0,
				GOWAYWSSCompatible:   false,
				GOWAYWSSHTTPStatus:   403,
				GOWAYWSSErrorStage:   "status",
				GOWAYWSSErrorMessage: "Forbidden (HTTP 403)",
			},
		}, 1, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	if status.NewCandidates != 0 {
		t.Fatalf("HTTP 403 must NOT enter candidate pool, got new_candidates=%d", status.NewCandidates)
	}

	if _, exists := store.Get(targetIP); exists {
		t.Fatalf("HTTP 403 route %s must NOT exist in RouteStore", targetIP)
	}
}

func TestDiscoveryGOWAYWSS200Blocked(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.103"
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{
			{
				IP:                   targetIP,
				Port:                 443,
				TCPLatency:           25.0,
				GOWAYWSSCompatible:   false,
				GOWAYWSSHTTPStatus:   200,
				GOWAYWSSErrorStage:   "status",
				GOWAYWSSErrorMessage: "HTTP 200 (expected 101 Switching Protocols)",
			},
		}, 1, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	if status.NewCandidates != 0 {
		t.Fatalf("HTTP 200 must NOT enter candidate pool in GOWAY-WSS mode, got new_candidates=%d", status.NewCandidates)
	}

	if _, exists := store.Get(targetIP); exists {
		t.Fatalf("HTTP 200 route %s must NOT exist in RouteStore", targetIP)
	}
}

func TestDiscoveryGOWAYWSS404Blocked(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.104"
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{
			{
				IP:                   targetIP,
				Port:                 443,
				TCPLatency:           25.0,
				GOWAYWSSCompatible:   false,
				GOWAYWSSHTTPStatus:   404,
				GOWAYWSSErrorStage:   "status",
				GOWAYWSSErrorMessage: "Not Found (HTTP 404)",
			},
		}, 1, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	if status.NewCandidates != 0 {
		t.Fatalf("HTTP 404 must NOT enter candidate pool, got new_candidates=%d", status.NewCandidates)
	}

	if _, exists := store.Get(targetIP); exists {
		t.Fatalf("HTTP 404 route %s must NOT exist in RouteStore", targetIP)
	}
}

func TestDiscoveryCFSTAllowsHTTPSWithoutWSS(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	if cfg.Profile.Type != ProfileCFST {
		t.Fatalf("expected ProfileCFST default")
	}
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.105"
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		if profile.Type != ProfileCFST {
			t.Fatalf("scanFunc received non-CFST profile: %s", profile.Type)
		}
		return []NodeResult{
			{
				IP:                 targetIP,
				Port:               443,
				TCPLatency:         28.0,
				Colo:               "SJC",
				SingleSpeed:        12.5,
				GOWAYWSSCompatible: false, // CFST mode does not set this
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
		t.Fatalf("expected 1 new candidate for valid CFST HTTPS node, got %d", status.NewCandidates)
	}

	rec, exists := store.Get(targetIP)
	if !exists {
		t.Fatalf("route %s must exist in RouteStore", targetIP)
	}
	if rec.Metrics.Tier != TierCandidate {
		t.Fatalf("expected TierCandidate, got %s", rec.Metrics.Tier)
	}
	if rec.Metrics.GOWAYWSSCompatible {
		t.Fatalf("ProfileCFST node must have GOWAYWSSCompatible=false")
	}
}

func TestDiscoveryGOWAYWSSNotBypassedByHTTPSSuccess(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.106"
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		// Simulate: TCP succeeded, HTTPS succeeded (speed > 0, Colo returned), BUT WSS returned HTTP 403!
		return []NodeResult{
			{
				IP:                   targetIP,
				Port:                 443,
				TCPLatency:           18.0,
				Colo:                 "NRT",
				DownloadSpeed:        25.0,
				SingleSpeed:          25.0,
				GOWAYWSSCompatible:   false, // WSS 403!
				GOWAYWSSHTTPStatus:   403,
				GOWAYWSSErrorStage:   "status",
				GOWAYWSSErrorMessage: "Forbidden (HTTP 403)",
			},
		}, 1, 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	// Must NOT be bypassed by HTTPS success!
	if status.NewCandidates != 0 {
		t.Fatalf("CRITICAL BYPASS: node with GOWAYWSSCompatible=false was admitted into candidates due to HTTPS success! new_candidates=%d", status.NewCandidates)
	}

	if _, exists := store.Get(targetIP); exists {
		t.Fatalf("CRITICAL BYPASS: node %s exists in RouteStore despite failing WSS check", targetIP)
	}
}

func TestDiscoveryExistingRouteBypassBlocked(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.107"
	// Insert an existing route currently in TierActive and marked healthy
	store.UpsertRoute(RouteMetrics{
		ID:                 GenerateRouteID(targetIP, 443),
		IP:                 targetIP,
		Port:               443,
		Tier:               TierActive,
		Health:             HealthHealthy,
		HandshakeSuccess:   true,
		GOWAYWSSCompatible: true,
		LastTested:         time.Now().Add(-5 * time.Minute),
	})

	// Discovery scans and finds this IP, but this round it fails WSS (e.g. HTTP 502)
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{
			{
				IP:                   targetIP,
				Port:                 443,
				TCPLatency:           22.0,
				GOWAYWSSCompatible:   false,
				GOWAYWSSHTTPStatus:   502,
				GOWAYWSSErrorStage:   "status",
				GOWAYWSSErrorMessage: "Bad Gateway (HTTP 502)",
			},
		}, 1, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	if status.ExistingRoutes != 0 {
		t.Fatalf("failed route must NOT be counted as valid existing route, got %d", status.ExistingRoutes)
	}

	rec, exists := store.Get(targetIP)
	if !exists {
		t.Fatalf("expected record to exist in store")
	}
	if rec.Metrics.GOWAYWSSCompatible {
		t.Fatalf("existing route must NOT retain GOWAYWSSCompatible=true after failing WSS in discovery")
	}
	if rec.Metrics.HandshakeSuccess {
		t.Fatalf("existing route must NOT retain HandshakeSuccess=true after failing WSS in discovery")
	}
	if rec.Metrics.Health == HealthHealthy {
		t.Fatalf("existing route must NOT retain HealthHealthy after failure")
	}
}

func TestDiscoveryGOWAYWSSFailureUpdatesExistingRoute(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.110"
	store.UpsertRoute(RouteMetrics{
		ID:                 GenerateRouteID(targetIP, 443),
		IP:                 targetIP,
		Port:               443,
		Tier:               TierCandidate,
		Health:             HealthHealthy,
		HandshakeSuccess:   true,
		GOWAYWSSCompatible: true,
		LastTested:         time.Now().Add(-5 * time.Minute),
	})

	dm.ipProvider = func() []string {
		return []string{targetIP}
	}
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{}, 1, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	if status.ExistingRoutes != 0 {
		t.Fatalf("failed route must NOT be counted as valid existing route, got %d", status.ExistingRoutes)
	}

	rec, exists := store.Get(targetIP)
	if !exists {
		t.Fatalf("expected record to exist in store")
	}
	if rec.Metrics.GOWAYWSSCompatible {
		t.Fatalf("expected GOWAYWSSCompatible == false, got true")
	}
	if rec.Metrics.HandshakeSuccess {
		t.Fatalf("expected HandshakeSuccess == false, got true")
	}
}

func TestDiscoveryGOWAYWSSFailureRecovery(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	targetIP := "198.51.100.111"
	store.UpsertRoute(RouteMetrics{
		ID:                 GenerateRouteID(targetIP, 443),
		IP:                 targetIP,
		Port:               443,
		Tier:               TierCandidate,
		Health:             HealthHealthy,
		HandshakeSuccess:   true,
		GOWAYWSSCompatible: true,
		LastTested:         time.Now().Add(-5 * time.Minute),
	})

	dm.ipProvider = func() []string {
		return []string{targetIP}
	}

	// Round 1: WSS fails, validNodes is empty
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{}, 1, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce round 1 failed: %v", err)
	}

	rec, exists := store.Get(targetIP)
	if !exists {
		t.Fatalf("expected record to exist in store after round 1")
	}
	if rec.Metrics.GOWAYWSSCompatible {
		t.Fatalf("round 1: expected GOWAYWSSCompatible == false, got true")
	}

	// Round 2: WSS succeeds with HTTP 101
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{
			{
				IP:                 targetIP,
				Port:               443,
				TCPLatency:         25.0,
				GOWAYWSSCompatible: true,
				GOWAYWSSHTTPStatus: 101,
				GOWAYWSSLatency:    35.0,
			},
		}, 1, 1
	}

	_, err = dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce round 2 failed: %v", err)
	}

	rec, exists = store.Get(targetIP)
	if !exists {
		t.Fatalf("expected record to exist in store after round 2")
	}
	if !rec.Metrics.GOWAYWSSCompatible {
		t.Fatalf("round 2 recovery: expected GOWAYWSSCompatible == true, got false")
	}
	if !rec.Metrics.HandshakeSuccess {
		t.Fatalf("round 2 recovery: expected HandshakeSuccess == true, got false")
	}
	if rec.Metrics.GOWAYWSSHTTPStatus != 101 {
		t.Fatalf("round 2 recovery: expected HTTPStatus == 101, got %d", rec.Metrics.GOWAYWSSHTTPStatus)
	}
}

func TestDiscoveryGOWAYWSSDoesNotModifyUnscannedRoute(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.example.com", "/pyway", "sni.example.com", 443)
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)

	ipA := "198.51.100.120"
	ipB := "198.51.100.121"

	store.UpsertRoute(RouteMetrics{
		ID:                 GenerateRouteID(ipA, 443),
		IP:                 ipA,
		Port:               443,
		Tier:               TierActive,
		Health:             HealthHealthy,
		HandshakeSuccess:   true,
		GOWAYWSSCompatible: true,
		LastTested:         time.Now().Add(-5 * time.Minute),
	})
	store.UpsertRoute(RouteMetrics{
		ID:                 GenerateRouteID(ipB, 443),
		IP:                 ipB,
		Port:               443,
		Tier:               TierActive,
		Health:             HealthHealthy,
		HandshakeSuccess:   true,
		GOWAYWSSCompatible: true,
		LastTested:         time.Now().Add(-5 * time.Minute),
	})

	// Only ipA is in scanned ips
	dm.ipProvider = func() []string {
		return []string{ipA}
	}
	// ipA fails WSS (validNodes empty)
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return []NodeResult{}, 1, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := dm.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	recA, existsA := store.Get(ipA)
	if !existsA {
		t.Fatalf("expected route A to exist")
	}
	if recA.Metrics.GOWAYWSSCompatible {
		t.Fatalf("scanned route A must have GOWAYWSSCompatible == false")
	}

	recB, existsB := store.Get(ipB)
	if !existsB {
		t.Fatalf("expected unscanned route B to exist")
	}
	if !recB.Metrics.GOWAYWSSCompatible {
		t.Fatalf("unscanned route B must remain GOWAYWSSCompatible == true")
	}
	if !recB.Metrics.HandshakeSuccess {
		t.Fatalf("unscanned route B must remain HandshakeSuccess == true")
	}
}

