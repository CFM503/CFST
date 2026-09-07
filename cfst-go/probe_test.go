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
