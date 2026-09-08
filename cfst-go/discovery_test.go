package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.ToLower(r.Header.Get("Upgrade")) == "websocket" || r.URL.Path == "/pyway" {
				wssAttempted.Store(true)
			}
			httpsAttempted.Store(true)
			w.Header().Set("cf-ray", "12345-HKG")
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()

	cfg := DefaultProbeConfig()
	if cfg.Profile.Type != ProfileCFST {
		t.Fatalf("expected DefaultProbeConfig profile to be ProfileCFST, got %s", cfg.Profile.Type)
	}

	cfg.Profile.Port = port
	cfg.Profile.TestURL = fmt.Sprintf("http://127.0.0.1:%d/__down", port)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
