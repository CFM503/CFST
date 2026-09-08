package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 1. TestProbeSchedulerCFSTUsesHTTPS verifies that under ProfileCFST,
// the scheduler performs standard HTTPS checks and never sends WSS upgrades or /pyway requests.
func TestProbeSchedulerCFSTUsesHTTPS(t *testing.T) {
	srv := startWSSMockServer(t, 200, "OK")
	defer srv.close()

	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileCFST()
	cfg.Profile.Port = srv.port
	cfg.Profile.TestURL = fmt.Sprintf("https://127.0.0.1:%d/cfst-check", srv.port)
	cfg.Profile.SNI = "127.0.0.1"
	cfg.ActiveInterval = 50 * time.Millisecond
	cfg.QuickDuration = 1
	cfg.FullDuration = 2

	scheduler := NewProbeScheduler(store, cfg)

	store.UpsertRoute(RouteMetrics{
		IP:         "127.0.0.1",
		Port:       srv.port,
		Tier:       TierActive,
		Health:     HealthHealthy,
		LastTested: time.Now().Add(-1 * time.Minute),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	scheduler.evaluateAndSchedule(ctx, time.Now())

	var lastM RouteMetrics
	var completed bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := store.GetMetrics("127.0.0.1"); ok && !m.LastTested.IsZero() && time.Since(m.LastTested) < 1*time.Second {
			lastM = m
			completed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !completed {
		t.Fatalf("probe was not completed within deadline")
	}

	_, tlsCount, _, _, _, upgrade := srv.snapshot()
	if tlsCount == 0 {
		t.Fatalf("expected TLS handshake to occur for CFST HTTPS check")
	}
	if upgrade == "websocket" {
		t.Fatalf("ProfileCFST must NEVER send Upgrade: websocket, got %q", upgrade)
	}
	if lastM.GOWAYWSSCompatible {
		t.Fatalf("ProfileCFST must NEVER mark route as GOWAYWSSCompatible")
	}
	if !lastM.HandshakeSuccess {
		t.Fatalf("expected HandshakeSuccess=true for valid HTTPS 200 check")
	}
}

// 2. TestProbeSchedulerGOWAYWSSUsesWSS verifies that under ProfileGOWAYWSS,
// the scheduler strictly runs GOWAY WSS handshakes with configured Host, SNI, Path, and Port.
func TestProbeSchedulerGOWAYWSSUsesWSS(t *testing.T) {
	srv := startWSSMockServer(t, 101, "")
	defer srv.close()

	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("wss.cfst.internal", "/pyway", "sni.cfst.internal", srv.port)
	cfg.ActiveInterval = 50 * time.Millisecond

	scheduler := NewProbeScheduler(store, cfg)

	store.UpsertRoute(RouteMetrics{
		IP:         "127.0.0.1",
		Port:       srv.port,
		Tier:       TierActive,
		Health:     HealthHealthy,
		LastTested: time.Now().Add(-1 * time.Minute),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	scheduler.evaluateAndSchedule(ctx, time.Now())

	var lastM RouteMetrics
	var completed bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := store.GetMetrics("127.0.0.1"); ok && !m.LastTested.IsZero() && time.Since(m.LastTested) < 1*time.Second {
			lastM = m
			completed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !completed {
		t.Fatalf("probe was not completed within deadline")
	}

	_, _, gotSNI, gotHost, gotPath, upgrade := srv.snapshot()
	if upgrade != "websocket" {
		t.Fatalf("ProfileGOWAYWSS must send Upgrade: websocket, got %q", upgrade)
	}
	if gotHost != "wss.cfst.internal" {
		t.Fatalf("expected Host %q, got %q", "wss.cfst.internal", gotHost)
	}
	if gotSNI != "sni.cfst.internal" {
		t.Fatalf("expected SNI %q, got %q", "sni.cfst.internal", gotSNI)
	}
	if gotPath != "/pyway" {
		t.Fatalf("expected Path %q, got %q", "/pyway", gotPath)
	}
	if !lastM.GOWAYWSSCompatible {
		t.Fatalf("expected GOWAYWSSCompatible=true on HTTP 101")
	}
	if lastM.GOWAYWSSHTTPStatus != 101 {
		t.Fatalf("expected GOWAYWSSHTTPStatus=101, got %d", lastM.GOWAYWSSHTTPStatus)
	}
	if !lastM.HandshakeSuccess {
		t.Fatalf("expected HandshakeSuccess=true")
	}
}

// 3. TestProbeOnceCFSTDoesNotUseWSS ensures a single ProbeOnce invocation in CFST mode does not trigger WSS.
func TestProbeOnceCFSTDoesNotUseWSS(t *testing.T) {
	srv := startWSSMockServer(t, 200, "OK")
	defer srv.close()

	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileCFST()
	cfg.Profile.Port = srv.port
	cfg.Profile.TestURL = fmt.Sprintf("https://127.0.0.1:%d/__down", srv.port)
	cfg.Profile.SNI = "127.0.0.1"

	scheduler := NewProbeScheduler(store, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	m, err := scheduler.ProbeOnce(ctx, "127.0.0.1", false)
	if err != nil {
		t.Fatalf("ProbeOnce failed: %v", err)
	}

	_, _, _, _, gotPath, upgrade := srv.snapshot()
	if upgrade == "websocket" {
		t.Fatalf("ProbeOnce with ProfileCFST must NOT send Upgrade: websocket")
	}
	if gotPath == "/pyway" {
		t.Fatalf("ProbeOnce with ProfileCFST must NOT request /pyway")
	}
	if m.GOWAYWSSCompatible {
		t.Fatalf("m.GOWAYWSSCompatible must be false under ProfileCFST")
	}
}

// 4. TestProbeOnceGOWAYWSSDoesNotUseHTTPS ensures a single ProbeOnce invocation in GOWAY WSS mode does not fall back to HTTPS.
func TestProbeOnceGOWAYWSSDoesNotUseHTTPS(t *testing.T) {
	// Server responds with 200 OK (standard HTTPS response, but NOT WSS 101)
	srv := startWSSMockServer(t, 200, "Standard HTTPS 200 OK")
	defer srv.close()

	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("gw.example.com", "/pyway", "sni.example.com", srv.port)

	scheduler := NewProbeScheduler(store, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	m, err := scheduler.ProbeOnce(ctx, "127.0.0.1", false)
	if err != nil {
		t.Fatalf("ProbeOnce unexpected error: %v", err)
	}

	_, _, _, _, _, upgrade := srv.snapshot()
	if upgrade != "websocket" {
		t.Fatalf("ProfileGOWAYWSS must request WebSocket upgrade, got %q", upgrade)
	}

	// Must NOT treat 200 as success! Must NOT fall back to standard HTTPS!
	if m.HandshakeSuccess {
		t.Fatalf("m.HandshakeSuccess must be false when server returns 200 instead of 101")
	}
	if m.GOWAYWSSCompatible {
		t.Fatalf("m.GOWAYWSSCompatible must be false on status 200")
	}
	if m.GOWAYWSSHTTPStatus != 200 {
		t.Fatalf("expected GOWAYWSSHTTPStatus=200, got %d", m.GOWAYWSSHTTPStatus)
	}
	if m.GOWAYWSSErrorStage != "status" {
		t.Fatalf("expected error stage 'status', got %q", m.GOWAYWSSErrorStage)
	}
}

// 5. TestTriggerOnDemandGOWAYWSSUsesWSS ensures on-demand triggered probes strictly adhere to ProfileGOWAYWSS.
func TestTriggerOnDemandGOWAYWSSUsesWSS(t *testing.T) {
	srv := startWSSMockServer(t, 101, "")
	defer srv.close()

	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("demand.test.internal", "/custom-demand", "demand-sni.internal", srv.port)

	scheduler := NewProbeScheduler(store, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler.Start(ctx)
	defer scheduler.Stop()

	scheduler.TriggerOnDemand("127.0.0.1")

	// Wait for on-demand trigger to process
	var found bool
	var m RouteMetrics
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recM, ok := store.GetMetrics("127.0.0.1"); ok && recM.GOWAYWSSCompatible {
			m = recM
			found = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !found {
		t.Fatalf("TriggerOnDemand did not successfully probe and record GOWAY WSS metric within deadline")
	}

	_, _, gotSNI, gotHost, gotPath, upgrade := srv.snapshot()
	if upgrade != "websocket" {
		t.Fatalf("TriggerOnDemand must use WSS upgrade, got %q", upgrade)
	}
	if gotHost != "demand.test.internal" {
		t.Fatalf("expected Host %q, got %q", "demand.test.internal", gotHost)
	}
	if gotSNI != "demand-sni.internal" {
		t.Fatalf("expected SNI %q, got %q", "demand-sni.internal", gotSNI)
	}
	if gotPath != "/custom-demand" {
		t.Fatalf("expected Path %q, got %q", "/custom-demand", gotPath)
	}
	if !m.GOWAYWSSCompatible || m.GOWAYWSSHTTPStatus != 101 {
		t.Fatalf("expected GOWAYWSSCompatible=true and status 101, got %+v", m)
	}
}

// 6. TestGOWAYWSSFailureDoesNotBecomeSuccess verifies that when HTTP status != 101,
// WSS compatible is false, subsequent speed tests are aborted, and the route is not valid.
func TestGOWAYWSSFailureDoesNotBecomeSuccess(t *testing.T) {
	statusCodes := []int{200, 403, 502}

	for _, code := range statusCodes {
		t.Run(fmt.Sprintf("Status_%d", code), func(t *testing.T) {
			srv := startWSSMockServer(t, code, fmt.Sprintf("Error %d", code))
			defer srv.close()

			store := NewRouteStore()
			cfg := DefaultProbeConfig()
			cfg.Profile = NewProfileGOWAYWSS("fail.test.internal", "/pyway", "fail-sni.internal", srv.port)

			scheduler := NewProbeScheduler(store, cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			target := ResolveProbeTarget(cfg, "127.0.0.1", srv.port)
			res := scheduler.ExecuteLayeredProbeWithSnapshot(ctx, target, cfg, true, true)

			// 1. WSS compatible must be false
			if res.GOWAYWSSCompatible {
				t.Fatalf("Status %d: res.GOWAYWSSCompatible must be false", code)
			}
			if res.HandshakeSuccess {
				t.Fatalf("Status %d: res.HandshakeSuccess must be false", code)
			}
			if res.Success {
				t.Fatalf("Status %d: res.Success must be false", code)
			}
			if res.GOWAYWSSHTTPStatus != code {
				t.Fatalf("Status %d: expected res.GOWAYWSSHTTPStatus=%d, got %d", code, code, res.GOWAYWSSHTTPStatus)
			}

			// 2. Must not run subsequent speed tests
			if res.L3Executed {
				t.Fatalf("Status %d: L3 speed probe must NOT be executed on WSS handshake failure", code)
			}
			if res.L4Executed {
				t.Fatalf("Status %d: L4 full speed test must NOT be executed on WSS handshake failure", code)
			}
			if res.DownloadSpeed > 0 || res.SingleSpeed > 0 {
				t.Fatalf("Status %d: speed metrics must be 0 on failure, got DownloadSpeed=%.2f", code, res.DownloadSpeed)
			}

			// 3. Must not become a valid/healthy route in store
			m, err := scheduler.ProbeOnce(ctx, "127.0.0.1", false)
			if err != nil {
				t.Fatalf("ProbeOnce failed: %v", err)
			}
			if m.GOWAYWSSCompatible {
				t.Fatalf("Status %d: m.GOWAYWSSCompatible must be false", code)
			}
			if m.HandshakeSuccess {
				t.Fatalf("Status %d: m.HandshakeSuccess must be false", code)
			}
			if m.Health == HealthHealthy {
				t.Fatalf("Status %d: Health must not be HEALTHY after WSS handshake failure", code)
			}
			if m.Recommendation == RecBest || m.Recommendation == RecGood {
				t.Fatalf("Status %d: Recommendation must not be BEST/GOOD, got %v", code, m.Recommendation)
			}

			// Excluded from GetBest()
			best := store.GetBest(10, "")
			for _, r := range best {
				if r.IP == "127.0.0.1" {
					t.Fatalf("Status %d: route must NOT be returned in GetBest()", code)
				}
			}
		})
	}
}

// 7. TestGOWAYWSSRecovery verifies that when a previously failing IP recovers and returns 101,
// its compatible status, handshake success, and health recover properly.
func TestGOWAYWSSRecovery(t *testing.T) {
	// First: start server returning 502 Bad Gateway
	srv := startWSSMockServer(t, 502, "Bad Gateway")
	defer srv.close()

	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.Profile = NewProfileGOWAYWSS("recov.test.internal", "/pyway", "recov-sni.internal", srv.port)

	scheduler := NewProbeScheduler(store, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Initial probe: fails with 502
	m1, err := scheduler.ProbeOnce(ctx, "127.0.0.1", false)
	if err != nil {
		t.Fatalf("initial ProbeOnce failed: %v", err)
	}
	if m1.GOWAYWSSCompatible {
		t.Fatalf("initial probe must have GOWAYWSSCompatible=false")
	}
	if m1.HandshakeSuccess {
		t.Fatalf("initial probe must have HandshakeSuccess=false")
	}
	if m1.Health == HealthHealthy {
		t.Fatalf("initial probe must not be HealthHealthy")
	}

	// 2. Server recovers to 101 Switching Protocols
	srv.mu.Lock()
	srv.statusCode = 101
	srv.responseBody = ""
	srv.mu.Unlock()

	// 3. Second probe: succeeds with 101
	m2, err := scheduler.ProbeOnce(ctx, "127.0.0.1", false)
	if err != nil {
		t.Fatalf("recovery ProbeOnce failed: %v", err)
	}

	// State must be properly recovered
	if !m2.GOWAYWSSCompatible {
		t.Fatalf("recovered probe must have GOWAYWSSCompatible=true")
	}
	if !m2.HandshakeSuccess {
		t.Fatalf("recovered probe must have HandshakeSuccess=true")
	}
	if m2.GOWAYWSSHTTPStatus != 101 {
		t.Fatalf("expected GOWAYWSSHTTPStatus=101, got %d", m2.GOWAYWSSHTTPStatus)
	}
	if m2.ConsecutiveSuccess < 1 {
		t.Fatalf("expected ConsecutiveSuccess >= 1, got %d", m2.ConsecutiveSuccess)
	}
	if m2.ConsecutiveFails != 0 {
		t.Fatalf("expected ConsecutiveFails=0 after recovery, got %d", m2.ConsecutiveFails)
	}

	// Store record must also be updated
	storeMetrics, ok := store.GetMetrics("127.0.0.1")
	if !ok {
		t.Fatalf("route missing from store")
	}
	if !storeMetrics.GOWAYWSSCompatible {
		t.Fatalf("store metrics must have GOWAYWSSCompatible=true")
	}
	if !storeMetrics.HandshakeSuccess {
		t.Fatalf("store metrics must have HandshakeSuccess=true")
	}
}
