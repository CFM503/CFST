package main

import (
	"context"
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
		isL3 := (int(c) % cfg.L3ProbeCycle == 0)
		isL4 := (int(c) % cfg.FullSpeedCycle == 0)

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
	if target.Host != "my-vps.com" {
		t.Fatalf("expected host my-vps.com, got %s", target.Host)
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
