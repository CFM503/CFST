package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRouteRecordHistoryAndPeakHours(t *testing.T) {
	baseTime := time.Date(2026, 9, 7, 20, 0, 0, 0, time.Local) // 20:00 (peak hour)
	rec := NewRouteRecord(RouteMetrics{IP: "1.1.1.1", Port: 443})

	// Add 5 samples at 20:00 with poor network metrics (high loss, speed drop)
	for i := 0; i < 5; i++ {
		rec.AddSample(MeasurementSample{
			Timestamp:  baseTime.Add(time.Duration(i) * time.Minute),
			Speed:      10.0,
			MinSpeed:   5.0,
			RTT:        120.0,
			PacketLoss: 0.15,
			Jitter:     25.0,
			Stability:  60.0,
			Success:    true,
		})
	}

	// Add 1 failed sample at 20:10
	rec.AddSample(MeasurementSample{
		Timestamp: baseTime.Add(10 * time.Minute),
		Success:   false,
	})

	// Check 15m window
	w15 := rec.CalcWindowStats(15*time.Minute, baseTime.Add(11*time.Minute))
	if w15.Count != 6 {
		t.Fatalf("expected 6 samples in 15m window, got %d", w15.Count)
	}
	if w15.FailRate == 0 {
		t.Fatal("expected non-zero fail rate in 15m window")
	}

	// Check PeakHour stats for hour 20
	hs := rec.PeakHours[20]
	if hs.Count != 6 || hs.FailCount != 1 {
		t.Fatalf("unexpected peak hour 20 stats: count=%d, fails=%d", hs.Count, hs.FailCount)
	}
	if hs.AvgSpeed <= 0 || hs.AvgLoss <= 0 {
		t.Fatalf("expected positive avg speed and loss in hour 20: %+v", hs)
	}

	// Peak hour penalty must be triggered for hour 20
	penalty := rec.PeakHourPenalty(20)
	if penalty <= 0 {
		t.Fatalf("expected positive peak hour penalty, got %.2f", penalty)
	}

	// Hour 14 should have no samples and zero penalty
	if rec.PeakHourPenalty(14) != 0 {
		t.Fatalf("expected 0 penalty for untested hour 14, got %.2f", rec.PeakHourPenalty(14))
	}
}

func TestRouteStoreAndPersistence(t *testing.T) {
	store := NewRouteStore()

	m1 := RouteMetrics{
		ID:         "route-1-1-1-1-443",
		IP:         "1.1.1.1",
		Port:       443,
		Colo:       "HKG",
		Tier:       TierActive,
		Health:     HealthHealthy,
		FinalScore: 92.5,
		LastTested: time.Now(),
	}
	m2 := RouteMetrics{
		ID:         "route-1-0-0-1-443",
		IP:         "1.0.0.1",
		Port:       443,
		Colo:       "NRT",
		Tier:       TierStandby,
		Health:     HealthHealthy,
		FinalScore: 88.0,
		LastTested: time.Now(),
	}

	store.UpsertRoute(m1)
	store.UpsertRoute(m2)

	all := store.GetAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(all))
	}

	best := store.GetBest(1, "")
	if len(best) != 1 || best[0].IP != "1.1.1.1" {
		t.Fatalf("expected 1.1.1.1 as best route, got %+v", best)
	}

	// Test SetTier
	if !store.SetTier("1.0.0.1", TierActive) {
		t.Fatal("failed to set tier")
	}
	rec, _ := store.Get("1.0.0.1")
	if rec.Metrics.Tier != TierActive {
		t.Fatalf("expected tier ACTIVE, got %s", rec.Metrics.Tier)
	}

	// Test Snapshot persistence
	tmpDir := t.TempDir()
	snapshotPath := filepath.Join(tmpDir, "test_snapshot.json")

	if err := store.SaveSnapshot(snapshotPath); err != nil {
		t.Fatalf("failed to save snapshot: %v", err)
	}

	// Load into fresh store
	newStore := NewRouteStore()
	if err := newStore.LoadSnapshot(snapshotPath); err != nil {
		t.Fatalf("failed to load snapshot: %v", err)
	}

	loadedAll := newStore.GetAll()
	if len(loadedAll) != 2 {
		t.Fatalf("expected 2 loaded routes, got %d", len(loadedAll))
	}

	_ = os.Remove(snapshotPath)
}
