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
