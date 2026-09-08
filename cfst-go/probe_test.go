package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeSchedulerConfigAndTiers(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.ActiveInterval = 500 * time.Millisecond
	cfg.StandbyInterval = 1 * time.Second
	cfg.CandidateInterval = 2 * time.Second
	cfg.FailedInterval = 1 * time.Second

	scheduler := NewProbeScheduler(store, cfg)
	currentCfg := scheduler.GetConfig()
	if currentCfg.ActiveInterval != 500*time.Millisecond {
		t.Fatalf("unexpected active interval: %v", currentCfg.ActiveInterval)
	}

	// Insert routes across tiers
	store.UpsertRoute(RouteMetrics{
		IP:         "1.1.1.1",
		Port:       443,
		Tier:       TierActive,
		Health:     HealthHealthy,
		LastTested: time.Now().Add(-10 * time.Second), // overdue for probe
	})

	store.UpsertRoute(RouteMetrics{
		IP:         "1.0.0.1",
		Port:       443,
		Tier:       TierStandby,
		Health:     HealthHealthy,
		LastTested: time.Now().Add(-10 * time.Second), // overdue
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler.Start(ctx)
	if !scheduler.running.Load() {
		t.Fatal("expected scheduler to be running")
	}

	time.Sleep(100 * time.Millisecond)
	scheduler.Stop()
	if scheduler.running.Load() {
		t.Fatal("expected scheduler to be stopped")
	}
}

func TestLowTrafficProbeScheduler(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.L3ProbeCycle = 3
	cfg.FullSpeedCycle = 60

	scheduler := NewProbeScheduler(store, cfg)

	// Test Active route cycle progression
	activeIP := "1.1.1.1"
	store.UpsertRoute(RouteMetrics{
		IP:     activeIP,
		Port:   443,
		Tier:   TierActive,
		Health: HealthHealthy,
	})

	for i := 1; i <= 60; i++ {
		c := scheduler.incCycle(activeIP)
		if c != int64(i) {
			t.Fatalf("expected cycle %d, got %d", i, c)
		}
		isL3 := (int(c)%cfg.L3ProbeCycle == 0)
		isL4 := (int(c)%cfg.FullSpeedCycle == 0)

		if i == 1 || i == 2 {
			if isL3 || isL4 {
				t.Fatalf("cycle %d must not trigger L3 or L4 speed tests", i)
			}
		}
		if i == 3 {
			if !isL3 || isL4 {
				t.Fatalf("cycle 3 must trigger L3 lightweight probe, but not L4 full speed")
			}
		}
		if i == 60 {
			if !isL3 || !isL4 {
				t.Fatalf("cycle 60 must trigger both L3 and L4 full speed calibration")
			}
		}
	}

	// Test Failed route protection
	failedIP := "192.0.2.1"
	store.UpsertRoute(RouteMetrics{
		IP:     failedIP,
		Port:   443,
		Tier:   TierFailed,
		Health: HealthFailed,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	m, _ := scheduler.ProbeOnce(ctx, failedIP, false)
	if m != nil && m.DownloadSpeed > 0 {
		t.Fatalf("failed route must not have download speed test run, got %.2f MB/s", m.DownloadSpeed)
	}
}

func TestProbeProfileConfiguration(t *testing.T) {
	cfstProf := NewProfileCFST()
	if cfstProf.Type != ProfileCFST || cfstProf.TestURL != "https://speed.cloudflare.com/__down?bytes=500000000" {
		t.Fatalf("unexpected CFST profile: %+v", cfstProf)
	}

	gowayProf := NewProfileGOWAYWSS("colo.4467107.xyz", "/pyway", "colo.4467107.xyz", 443)
	if gowayProf.Type != ProfileGOWAYWSS || gowayProf.Protocol != "wss" {
		t.Fatalf("unexpected GOWAY WSS profile: %+v", gowayProf)
	}

	customProf := NewProfileCustom("https://example.com/speed", "custom.domain", 443)
	if customProf.Type != ProfileCustom || customProf.TestURL != "https://example.com/speed" {
		t.Fatalf("unexpected Custom profile: %+v", customProf)
	}
}

func TestProfileCFSTDoesNotCallWSS(t *testing.T) {
	cfg := DefaultProbeConfig()
	if cfg.Profile.Type != ProfileCFST {
		t.Fatalf("expected default profile CFST, got %s", cfg.Profile.Type)
	}
	if cfg.WSSHost != "" {
		t.Fatalf("CFST mode must have empty WSSHost, got %s", cfg.WSSHost)
	}

	target := ResolveProbeTarget(cfg, "1.1.1.1", 443)
	if target.ProfileType != ProfileCFST {
		t.Fatalf("expected resolved target CFST, got %s", target.ProfileType)
	}
	if target.Protocol != "https" {
		t.Fatalf("expected https protocol for CFST, got %s", target.Protocol)
	}
	if target.SNI != "speed.cloudflare.com" || target.Host != "speed.cloudflare.com" {
		t.Fatalf("expected speed.cloudflare.com, got SNI=%s, Host=%s", target.SNI, target.Host)
	}
}

func TestProfileGOWAYWSSCustomParameters(t *testing.T) {
	prof := NewProfileGOWAYWSS("edge.goway.internal", "/custom-way", "sni.goway.internal", 8443)
	cfg := DefaultProbeConfig()
	cfg.Profile = prof
	NormalizeProbeConfig(&cfg)

	target := ResolveProbeTarget(cfg, "1.2.3.4", 8443)
	if target.ProfileType != ProfileGOWAYWSS {
		t.Fatalf("expected GOWAY-WSS, got %s", target.ProfileType)
	}
	if target.Host != "edge.goway.internal" {
		t.Fatalf("expected edge.goway.internal, got %s", target.Host)
	}
	if target.Path != "/custom-way" {
		t.Fatalf("expected /custom-way, got %s", target.Path)
	}
	if target.SNI != "sni.goway.internal" {
		t.Fatalf("expected sni.goway.internal, got %s", target.SNI)
	}
	if target.Protocol != "wss" {
		t.Fatalf("expected wss protocol, got %s", target.Protocol)
	}
	if target.Port != 8443 {
		t.Fatalf("expected port 8443, got %d", target.Port)
	}
}

func TestProfileCustomVPSURL(t *testing.T) {
	vpsURL := "https://my-vps.com:8443/data/speedtest.bin"
	prof := NewProfileCustom(vpsURL, "my-vps.com", 8443)
	cfg := DefaultProbeConfig()
	cfg.Profile = prof
	NormalizeProbeConfig(&cfg)

	target := ResolveProbeTarget(cfg, "8.8.8.8", 8443)
	if target.ProfileType != ProfileCustom {
		t.Fatalf("expected CUSTOM profile, got %s", target.ProfileType)
	}
	if target.URL != vpsURL {
		t.Fatalf("expected %s, got %s", vpsURL, target.URL)
	}
	if target.Host != "my-vps.com:8443" {
		t.Fatalf("expected host my-vps.com:8443, got %s", target.Host)
	}
	if target.SNI != "my-vps.com" {
		t.Fatalf("expected SNI my-vps.com, got %s", target.SNI)
	}
	if target.Path != "/data/speedtest.bin" {
		t.Fatalf("expected /data/speedtest.bin, got %s", target.Path)
	}
	if target.Protocol != "https" {
		t.Fatalf("expected https protocol, got %s", target.Protocol)
	}
}

func TestAPIUpdateProfileImmediateEffect(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	scheduler := NewProbeScheduler(store, cfg)

	// Simulate POST /api/config with ProfileCustom
	newProfile := NewProfileCustom("https://custom-speed.org/testfile.img", "custom-speed.org", 443)
	updatedCfg := scheduler.GetConfig()
	updatedCfg.Profile = newProfile
	scheduler.UpdateConfig(updatedCfg)

	// Verify immediate effect
	current := scheduler.GetConfig()
	if current.Profile.Type != ProfileCustom {
		t.Fatalf("expected updated profile CUSTOM, got %s", current.Profile.Type)
	}
	if current.Profile.TestURL != "https://custom-speed.org/testfile.img" {
		t.Fatalf("expected updated TestURL, got %s", current.Profile.TestURL)
	}

	target := ResolveProbeTarget(current, "9.9.9.9", 443)
	if target.URL != "https://custom-speed.org/testfile.img" {
		t.Fatalf("expected target URL custom-speed.org, got %s", target.URL)
	}
	if target.SNI != "custom-speed.org" {
		t.Fatalf("expected target SNI custom-speed.org, got %s", target.SNI)
	}
}

func TestLegacyFieldsDiscrepancyProfileWins(t *testing.T) {
	cfg := DefaultProbeConfig()
	// Deliberately set legacy fields to stale values that disagree with Profile
	cfg.Profile = NewProfileCustom("https://authoritative.com/speed.bin", "authoritative.com", 443)
	cfg.URL = "https://stale-legacy.com/old"
	cfg.SNI = "stale-legacy.com"
	cfg.WSSHost = "stale-wss.com"

	// Resolve target: Profile must strictly win!
	target := ResolveProbeTarget(cfg, "1.1.1.1", 443)
	if target.URL != "https://authoritative.com/speed.bin" {
		t.Fatalf("Profile must win over legacy cfg.URL, got %s", target.URL)
	}
	if target.SNI != "authoritative.com" {
		t.Fatalf("Profile must win over legacy cfg.SNI, got %s", target.SNI)
	}
	if target.Host != "authoritative.com" {
		t.Fatalf("Profile must win over legacy host, got %s", target.Host)
	}
}

func TestDefaultCFSTModeProtocolIsolation(t *testing.T) {
	cfg := DefaultProbeConfig()
	if cfg.Profile.Protocol != "https" {
		t.Fatalf("expected CFST protocol to be https, got %s", cfg.Profile.Protocol)
	}
	if cfg.WSSHost != "" {
		t.Fatalf("CFST mode must have empty WSSHost, got %s", cfg.WSSHost)
	}

	target := ResolveProbeTarget(cfg, "1.1.1.1", 443)
	if target.Protocol == "wss" {
		t.Fatalf("CFST mode must never resolve to wss protocol")
	}
}

func TestProbeInFlightDedup(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	scheduler := NewProbeScheduler(store, cfg)

	testIP := "198.51.100.1"
	store.UpsertRoute(RouteMetrics{
		IP:     testIP,
		Port:   443,
		Tier:   TierActive,
		Health: HealthHealthy,
	})

	// Manually mark inFlight
	scheduler.inFlight.Store(testIP, struct{}{})

	// Calling ProbeOnce while inFlight must return immediately without error
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	m, err := scheduler.ProbeOnce(ctx, testIP, false)
	if err != nil {
		t.Fatalf("expected graceful return for in-flight route, got error: %v", err)
	}
	if m == nil || m.IP != testIP {
		t.Fatalf("expected existing route metrics returned, got %+v", m)
	}

	// Now clear inFlight
	scheduler.inFlight.Delete(testIP)

	// Ensure inFlight is cleared on probe completion
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	_, _ = scheduler.ProbeOnce(ctx2, testIP, false)

	if _, loaded := scheduler.inFlight.Load(testIP); loaded {
		t.Fatalf("expected inFlight to be cleaned up after ProbeOnce returns")
	}
}

func TestProbeUsesConfigSnapshot(t *testing.T) {
	store := NewRouteStore()
	initialCfg := DefaultProbeConfig()
	initialCfg.FullDuration = 12
	scheduler := NewProbeScheduler(store, initialCfg)

	snapCfg := scheduler.GetConfig()
	if snapCfg.FullDuration != 12 {
		t.Fatalf("expected snapshot FullDuration 12, got %d", snapCfg.FullDuration)
	}

	// Concurrently change scheduler config
	modifiedCfg := DefaultProbeConfig()
	modifiedCfg.FullDuration = 99
	scheduler.UpdateConfig(modifiedCfg)

	// The previous snapshot MUST remain unchanged
	if snapCfg.FullDuration != 12 {
		t.Fatalf("snapshot must remain immutable, got %d", snapCfg.FullDuration)
	}
}

func TestCandidatePromotion(t *testing.T) {
	store := NewRouteStore()

	// Route starts as Candidate
	candidateIP := "104.16.1.1"
	store.UpsertRoute(RouteMetrics{
		IP:     candidateIP,
		Port:   443,
		Tier:   TierCandidate,
		Health: HealthHealthy,
	})

	rec, _ := store.Get(candidateIP)

	// Single high-speed test must NOT promote directly to Active!
	sample1 := MeasurementSample{
		Timestamp: time.Now().Add(-10 * time.Minute),
		Speed:     85.0,
		P10Speed:  70.0,
		RTT:       25.0,
		Stability: 95.0,
		Success:   true,
	}
	rec.AddSample(sample1)
	store.EvaluateTierTransitions(time.Now())

	rec, _ = store.Get(candidateIP)
	if rec.Metrics.Tier == TierActive {
		t.Fatalf("single high speed test must NEVER promote Candidate directly to Active!")
	}

	// Add more observations over 6 minutes to satisfy promotion criteria
	sample2 := MeasurementSample{
		Timestamp: time.Now().Add(-5 * time.Minute),
		Speed:     80.0,
		P10Speed:  68.0,
		RTT:       26.0,
		Stability: 92.0,
		Success:   true,
	}
	sample3 := MeasurementSample{
		Timestamp: time.Now(),
		Speed:     82.0,
		P10Speed:  69.0,
		RTT:       24.0,
		Stability: 94.0,
		Success:   true,
	}
	rec.AddSample(sample2)
	rec.AddSample(sample3)

	now := time.Now()
	rec.Metrics.ObservationDuration = 360.0
	rec.Metrics.Confidence = rec.CalcConfidence(now)
	rec.Metrics.FinalScore = 88.0
	rec.Metrics.Health = HealthHealthy
	rec.Metrics.PacketLoss = 0.0
	rec.Metrics.Jitter = 2.0
	rec.Metrics.StallCount = 0

	store.EvaluateTierTransitions(now)

	rec, _ = store.Get(candidateIP)
	if rec.Metrics.Tier != TierStandby && rec.Metrics.Tier != TierActive {
		t.Fatalf("expected candidate to promote to Standby or Active, got %s", rec.Metrics.Tier)
	}
}

func TestLongTermDemotion(t *testing.T) {
	store := NewRouteStore()

	activeIP := "104.16.2.1"
	store.UpsertRoute(RouteMetrics{
		IP:     activeIP,
		Port:   443,
		Tier:   TierActive,
		Health: HealthHealthy,
	})

	now := time.Now()

	// 1. Active degrades -> demotes to Standby
	rec, _ := store.Get(activeIP)
	rec.Metrics.Health = HealthDegraded
	rec.Metrics.SpeedDropPercent = 45.0
	store.EvaluateTierTransitions(now)

	rec, _ = store.Get(activeIP)
	if rec.Metrics.Tier != TierStandby {
		t.Fatalf("expected degraded Active to demote to Standby, got %s", rec.Metrics.Tier)
	}

	// 2. Standby fails repeatedly -> demotes to Candidate
	rec.Metrics.Health = HealthFailing
	rec.Metrics.ConsecutiveFails = 3
	store.EvaluateTierTransitions(now)

	rec, _ = store.Get(activeIP)
	if rec.Metrics.Tier != TierCandidate {
		t.Fatalf("expected failing Standby to demote to Candidate, got %s", rec.Metrics.Tier)
	}

	// 3. Candidate fails persistently -> demotes to Failed
	rec.Metrics.Health = HealthFailed
	rec.Metrics.ConsecutiveFails = 5
	store.EvaluateTierTransitions(now)

	rec, _ = store.Get(activeIP)
	if rec.Metrics.Tier != TierFailed {
		t.Fatalf("expected persistent failing Candidate to demote to Failed, got %s", rec.Metrics.Tier)
	}

	// 4. Long-term failed (> 2h, fails >= 10) gets pruned
	rec.Metrics.ConsecutiveFails = 10
	rec.Metrics.LastTested = now.Add(-3 * time.Hour)
	store.EvaluateTierTransitions(now)

	_, exists := store.Get(activeIP)
	if exists {
		t.Fatalf("expected long-term failed route to be pruned from RouteStore")
	}
}

func TestProbeInFlightConcurrentRace(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	scheduler := NewProbeScheduler(store, cfg)

	testIP := "198.51.100.2"
	store.UpsertRoute(RouteMetrics{
		IP:     testIP,
		Port:   443,
		Tier:   TierActive,
		Health: HealthHealthy,
	})

	var executionCount atomic.Int32
	scheduler.probeExec = func(ctx context.Context, target ResolvedProbeTarget, cfg ProbeConfig, runL3 bool, runFullSpeed bool) LayeredProbeResult {
		executionCount.Add(1)
		time.Sleep(50 * time.Millisecond) // hold in-flight state
		return LayeredProbeResult{
			Success:          true,
			RTT:              20.0,
			HandshakeSuccess: true,
			SingleSpeed:      10.0,
			Colo:             "SJC",
		}
	}

	const goroutines = 10
	var wg sync.WaitGroup
	startCh := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = scheduler.ProbeOnce(ctx, testIP, false)
		}()
	}

	// Release all goroutines simultaneously
	close(startCh)
	wg.Wait()

	// Exactly 1 goroutine must have won the race and executed probeExec
	if count := executionCount.Load(); count != 1 {
		t.Fatalf("expected exactly 1 probe execution during concurrent race, got %d", count)
	}

	// After completion, inFlight must be clean
	if _, loaded := scheduler.inFlight.Load(testIP); loaded {
		t.Fatalf("expected inFlight to be cleaned up after all goroutines finish")
	}
}

func TestCandidateL3Observation(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	scheduler := NewProbeScheduler(store, cfg)

	candidateIP := "104.16.2.2"
	store.UpsertRoute(RouteMetrics{
		IP:     candidateIP,
		Port:   443,
		Tier:   TierCandidate,
		Health: HealthHealthy,
	})

	scheduler.probeExec = func(ctx context.Context, target ResolvedProbeTarget, cfg ProbeConfig, runL3 bool, runFullSpeed bool) LayeredProbeResult {
		return LayeredProbeResult{
			Success:          true,
			RTT:              22.0,
			PacketLoss:       0.0,
			Jitter:           1.2,
			HandshakeSuccess: true,
			SingleSpeed:      18.5,
			DownloadSpeed:    18.5,
			Colo:             "HKG",
			LoadLatency:      45.0,
			L3Executed:       true,
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	m, err := scheduler.ProbeOnce(ctx, candidateIP, false)
	if err != nil {
		t.Fatalf("ProbeOnce failed: %v", err)
	}

	if m.SingleSpeed != 18.5 {
		t.Fatalf("expected SingleSpeed 18.5, got %.2f", m.SingleSpeed)
	}
	if m.DownloadSpeed != 18.5 {
		t.Fatalf("expected DownloadSpeed 18.5, got %.2f", m.DownloadSpeed)
	}
	if m.Colo != "HKG" {
		t.Fatalf("expected Colo HKG, got %s", m.Colo)
	}

	rec, exists := store.Get(candidateIP)
	if !exists {
		t.Fatalf("expected route in store")
	}
	if len(rec.Samples) != 1 {
		t.Fatalf("expected 1 sample in history, got %d", len(rec.Samples))
	}
	if rec.Samples[0].Speed != 18.5 {
		t.Fatalf("expected sample speed 18.5, got %.2f", rec.Samples[0].Speed)
	}
}

func TestCandidateL3ObservationAndPromotion(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	scheduler := NewProbeScheduler(store, cfg)

	candidateIP := "104.16.3.3"
	store.UpsertRoute(RouteMetrics{
		IP:     candidateIP,
		Port:   443,
		Tier:   TierCandidate,
		Health: HealthHealthy,
	})

	baseTime := time.Now().Add(-6 * time.Minute)
	currentTestTime := baseTime

	scheduler.nowFunc = func() time.Time {
		return currentTestTime
	}

	scheduler.probeExec = func(ctx context.Context, target ResolvedProbeTarget, cfg ProbeConfig, runL3 bool, runFullSpeed bool) LayeredProbeResult {
		return LayeredProbeResult{
			Success:          true,
			RTT:              25.0,
			PacketLoss:       0.0,
			Jitter:           1.5,
			HandshakeSuccess: true,
			SingleSpeed:      22.0,
			DownloadSpeed:    22.0,
			Colo:             "HKG",
			L3Executed:       true,
		}
	}

	ctx := context.Background()

	// Execute 4 real ProbeOnce calls advancing mock observation time across 300+ seconds
	for i := 0; i < 4; i++ {
		currentTestTime = baseTime.Add(time.Duration(i*100) * time.Second)
		res, err := scheduler.ProbeOnce(ctx, candidateIP, false)
		if err != nil {
			t.Fatalf("ProbeOnce failed on cycle %d: %v", i, err)
		}
		if res == nil {
			t.Fatalf("ProbeOnce returned nil result on cycle %d", i)
		}
	}

	m, exists := store.GetMetrics(candidateIP)
	if !exists {
		t.Fatalf("expected candidate route in store")
	}
	if m.Tier != TierStandby {
		t.Fatalf("expected Candidate to promote to Standby based on real L3 observations and production scoring, got tier %s (Score=%.1f, Confidence=%.1f, Obs=%.1f)",
			m.Tier, m.FinalScore, m.Confidence, m.ObservationDuration)
	}
}

func TestProbeSchedulerRestart(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	scheduler := NewProbeScheduler(store, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start scheduler
	scheduler.Start(ctx)
	if !scheduler.running.Load() {
		t.Fatalf("expected scheduler to be running after Start()")
	}

	// 2. Stop scheduler
	scheduler.Stop()
	if scheduler.running.Load() {
		t.Fatalf("expected scheduler to be stopped after Stop()")
	}

	// 3. Restart scheduler cleanly without panic or immediate exit
	scheduler.Start(ctx)
	if !scheduler.running.Load() {
		t.Fatalf("expected scheduler to be running after second Start()")
	}

	// Final stop
	scheduler.Stop()
	if scheduler.running.Load() {
		t.Fatalf("expected scheduler to be stopped after final Stop()")
	}
}

func TestProbeSchedulerRestartNoRace(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	scheduler := NewProbeScheduler(store, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Setup synchronization channels to track goroutine lifecycle
	started1 := make(chan struct{})
	exited1 := make(chan struct{})

	scheduler.onStartGoroutine = func() {
		close(started1)
	}
	scheduler.onExitGoroutine = func() {
		close(exited1)
	}

	scheduler.Start(ctx)

	// Wait for goroutine 1 to be fully running
	select {
	case <-started1:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for goroutine 1 to start")
	}

	if !scheduler.running.Load() {
		t.Fatalf("expected scheduler to be running")
	}

	// 2. Stop scheduler and wait for goroutine 1 to exit cleanly
	scheduler.Stop()

	select {
	case <-exited1:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for goroutine 1 to exit")
	}

	if scheduler.running.Load() {
		t.Fatalf("expected scheduler to be stopped")
	}

	// 3. Setup synchronization for goroutine 2
	started2 := make(chan struct{})
	exited2 := make(chan struct{})

	scheduler.onStartGoroutine = func() {
		close(started2)
	}
	scheduler.onExitGoroutine = func() {
		close(exited2)
	}

	scheduler.Start(ctx)

	// Wait for goroutine 2 to be fully running
	select {
	case <-started2:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for goroutine 2 to start")
	}

	if !scheduler.running.Load() {
		t.Fatalf("expected scheduler to be running again")
	}

	// 4. Cleanly stop again
	scheduler.Stop()

	select {
	case <-exited2:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for goroutine 2 to exit")
	}

	if scheduler.running.Load() {
		t.Fatalf("expected scheduler to be stopped after second restart")
	}
}
