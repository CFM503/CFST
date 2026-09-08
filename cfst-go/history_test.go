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

func TestPeakHourSelectionComparison(t *testing.T) {
	store := NewRouteStore()

	// Route A: Fast during day, but collapses at peak hour (PeakHourScore=40)
	mA := RouteMetrics{
		ID:            "route-A",
		IP:            "1.1.1.1",
		Port:          443,
		Colo:          "HKG",
		Tier:          TierActive,
		Health:        HealthHealthy,
		FinalScore:    92.0,
		PeakHourScore: 40.0,
		Confidence:    90.0,
		P10Speed:      10.0,
		LastTested:    time.Now(),
	}

	// Route B: Steady and reliable during peak hours (PeakHourScore=88)
	mB := RouteMetrics{
		ID:            "route-B",
		IP:            "1.0.0.1",
		Port:          443,
		Colo:          "HKG",
		Tier:          TierActive,
		Health:        HealthHealthy,
		FinalScore:    86.0,
		PeakHourScore: 88.0,
		Confidence:    90.0,
		P10Speed:      25.0,
		LastTested:    time.Now(),
	}

	store.UpsertRoute(mA)
	store.UpsertRoute(mB)

	// In Normal mode, Route A has higher FinalScore (92 vs 86)
	GlobalScoreEngine.SetMode(ModeNormal)
	bestNormal := store.GetBest(1, "")
	if len(bestNormal) != 1 || bestNormal[0].IP != "1.1.1.1" {
		t.Fatalf("expected Route A (1.1.1.1) in Normal mode, got %+v", bestNormal)
	}

	// Switch to Peak mode: Route B MUST outrank Route A due to superior peak hour resilience
	GlobalScoreEngine.SetMode(ModePeak)
	defer GlobalScoreEngine.SetMode(ModeNormal)

	bestPeak := store.GetBest(1, "")
	if len(bestPeak) != 1 || bestPeak[0].IP != "1.0.0.1" {
		t.Fatalf("Peak Mode Requirement Violated: Route B (1.0.0.1, peak score 88) must be selected over Route A (1.1.1.1, peak score 40), got %+v", bestPeak)
	}
}

func TestRouteStoreHealthFiltering(t *testing.T) {
	store := NewRouteStore()

	// Healthy route
	store.UpsertRoute(RouteMetrics{
		ID:         "route-healthy",
		IP:         "10.0.0.1",
		Port:       443,
		Health:     HealthHealthy,
		FinalScore: 75.0,
		LastTested: time.Now(),
	})

	// Failed route (high historical score, but dead right now)
	store.UpsertRoute(RouteMetrics{
		ID:         "route-failed",
		IP:         "10.0.0.2",
		Port:       443,
		Health:     HealthFailed,
		FinalScore: 99.0,
		LastTested: time.Now(),
	})

	// Failing route
	store.UpsertRoute(RouteMetrics{
		ID:         "route-failing",
		IP:         "10.0.0.3",
		Port:       443,
		Health:     HealthFailing,
		FinalScore: 95.0,
		LastTested: time.Now(),
	})

	// GetBest must NEVER return FAILED or FAILING routes, even if their historical scores were 99!
	best := store.GetBest(5, "")
	if len(best) != 1 || best[0].IP != "10.0.0.1" {
		t.Fatalf("expected only healthy route 10.0.0.1, got: %+v", best)
	}
}

