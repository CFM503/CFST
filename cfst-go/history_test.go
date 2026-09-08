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

func TestConfidenceTimeSpanGating(t *testing.T) {
	now := time.Now()
	recShort := NewRouteRecord(RouteMetrics{IP: "1.1.1.1", Port: 443})

	// 12 samples over 2 minutes (every 10s)
	for i := 0; i < 12; i++ {
		ts := now.Add(time.Duration(i*10) * time.Second)
		recShort.AddSample(MeasurementSample{
			Timestamp: ts,
			Speed:     50.0,
			P10Speed:  45.0,
			RTT:       20.0,
			Success:   true,
		})
	}
	confShort := recShort.CalcConfidence(now.Add(120 * time.Second))
	if confShort >= 50.0 {
		t.Fatalf("Confidence gating failed: 12 samples over 2 minutes must have confidence < 50%%, got %.2f%%", confShort)
	}

	recLong := NewRouteRecord(RouteMetrics{IP: "1.0.0.1", Port: 443})
	// 12 samples over 4 hours (e.g. every 20 minutes)
	for i := 0; i < 12; i++ {
		ts := now.Add(time.Duration(i*20) * time.Minute)
		recLong.AddSample(MeasurementSample{
			Timestamp: ts,
			Speed:     50.0,
			P10Speed:  45.0,
			RTT:       20.0,
			Success:   true,
		})
	}
	confLong := recLong.CalcConfidence(now.Add(220 * time.Minute))
	if confLong <= 70.0 {
		t.Fatalf("Confidence gating failed: 12 samples over 4 hours should have confidence > 70%%, got %.2f%%", confLong)
	}
}

func TestSpeedDropBaselineOrder(t *testing.T) {
	store := NewRouteStore()
	ip := "2.2.2.2"

	// Establish stable baseline around 50 MB/s
	now := time.Now()
	for i := 0; i < 10; i++ {
		m := RouteMetrics{
			IP:               ip,
			Port:             443,
			SingleSpeed:      50.0,
			P10Speed:         50.0,
			RTT:              20.0,
			HandshakeSuccess: true,
			LastTested:       now.Add(time.Duration(i) * time.Minute),
		}
		store.RecordProbeResult(m, true)
	}

	rec, ok := store.Get(ip)
	if !ok {
		t.Fatal("route record not found")
	}
	baseline := rec.EWMA.LongSnapshot().P10Speed
	if baseline < 45.0 {
		t.Fatalf("expected established baseline around 50 MB/s, got %.2f", baseline)
	}

	// Now a sudden probe with speed 30 MB/s comes in
	suddenDropMetric := RouteMetrics{
		IP:               ip,
		Port:             443,
		SingleSpeed:      30.0,
		P10Speed:         30.0,
		RTT:              20.0,
		HandshakeSuccess: true,
		LastTested:       now.Add(11 * time.Minute),
	}
	store.RecordProbeResult(suddenDropMetric, true)

	recAfter, _ := store.Get(ip)
	// Baseline was ~50, new sample was 30 -> drop should be ~40%
	if recAfter.Metrics.SpeedDropPercent < 35.0 || recAfter.Metrics.SpeedDropPercent > 45.0 {
		t.Fatalf("expected speed drop percent around 40%%, got %.2f%% (baseline was %.2f)", recAfter.Metrics.SpeedDropPercent, baseline)
	}
}

func TestPeakHourEWMAHistoricalDecay(t *testing.T) {
	rec := NewRouteRecord(RouteMetrics{IP: "3.3.3.3", Port: 443})

	// Yesterday at 20:00 (peak hour), bad network (speed 10 MB/s, loss 0.20)
	yesterdayPeak := time.Date(2026, 9, 7, 20, 0, 0, 0, time.Local)
	rec.AddSample(MeasurementSample{
		Timestamp:  yesterdayPeak,
		Speed:      10.0,
		PacketLoss: 0.20,
		Success:    true,
	})

	hsYesterday := rec.PeakHours[20]
	if hsYesterday.AvgSpeed != 10.0 {
		t.Fatalf("expected yesterday avg speed 10, got %.2f", hsYesterday.AvgSpeed)
	}

	// Today at 20:00 (> 12 hours later), excellent network (speed 90 MB/s, loss 0.0)
	todayPeak := time.Date(2026, 9, 8, 20, 0, 0, 0, time.Local)
	rec.AddSample(MeasurementSample{
		Timestamp:  todayPeak,
		Speed:      90.0,
		PacketLoss: 0.0,
		Success:    true,
	})

	hsToday := rec.PeakHours[20]
	// If it decayed and applied EWMA (alpha = 0.35 on new data, decayed old * 0.70):
	// Decayed old speed was 10.0 * 0.7 = 7.0
	// Updated speed = 0.35 * 90.0 + 0.65 * 7.0 = 31.5 + 4.55 = 36.05
	// Rather than a simple average (10+90)/2 = 50 with equal weights forever
	if hsToday.AvgSpeed <= 10.0 || hsToday.AvgSpeed >= 90.0 {
		t.Fatalf("expected smoothed EWMA speed reflecting decay, got %.2f", hsToday.AvgSpeed)
	}
	if hsToday.LastUpdated != todayPeak {
		t.Fatalf("expected LastUpdated to be updated to todayPeak, got %v", hsToday.LastUpdated)
	}
}

func TestHistory24hRetention(t *testing.T) {
	rec := NewRouteRecord(RouteMetrics{IP: "4.4.4.4", Port: 443})
	now := time.Now()

	// Add sample from 26 hours ago (outside 24h)
	rec.AddSample(MeasurementSample{
		Timestamp: now.Add(-26 * time.Hour),
		Speed:     20.0,
		Success:   true,
	})

	// Add sample from 20 hours ago (inside 24h)
	rec.AddSample(MeasurementSample{
		Timestamp: now.Add(-20 * time.Hour),
		Speed:     60.0,
		Success:   true,
	})

	// Add sample from 1 hour ago
	rec.AddSample(MeasurementSample{
		Timestamp: now.Add(-1 * time.Hour),
		Speed:     80.0,
		Success:   true,
	})

	// The sample from 26h ago should have been pruned by 24h cutoff
	if len(rec.Samples) != 2 {
		t.Fatalf("expected 2 samples retained within 24 hours, got %d", len(rec.Samples))
	}
	if rec.Samples[0].Speed != 60.0 {
		t.Fatalf("expected oldest retained sample to be 60.0 MB/s, got %.2f", rec.Samples[0].Speed)
	}
}

func TestStaleRouteProtection(t *testing.T) {
	store := NewRouteStore()
	now := time.Now()

	// Route 1: Tested 2 minutes ago, active, score 80
	store.UpsertRoute(RouteMetrics{
		IP:         "5.5.5.1",
		Port:       443,
		Tier:       TierActive,
		Health:     HealthHealthy,
		FinalScore: 80.0,
		Confidence: 80.0,
		LastTested: now.Add(-2 * time.Minute),
	})

	// Route 2: Tested 70 minutes ago, excellent historical score 95
	store.UpsertRoute(RouteMetrics{
		IP:         "5.5.5.2",
		Port:       443,
		Tier:       TierStandby,
		Health:     HealthHealthy,
		FinalScore: 95.0,
		Confidence: 90.0,
		LastTested: now.Add(-70 * time.Minute),
	})

	// Route 3: Tested 20 minutes ago (stale for standby, but < 60m)
	store.UpsertRoute(RouteMetrics{
		IP:         "5.5.5.3",
		Port:       443,
		Tier:       TierStandby,
		Health:     HealthHealthy,
		FinalScore: 85.0,
		Confidence: 80.0,
		LastTested: now.Add(-20 * time.Minute),
	})

	best := store.GetBest(10, "")
	// Route 2 (> 60m) must be completely excluded from GetBest
	for _, r := range best {
		if r.IP == "5.5.5.2" {
			t.Fatalf("Route 5.5.5.2 was untested for >60m and must NOT be in GetBest()")
		}
	}

	// Route 1 (recent, not stale) should beat Route 3 (stale, penalized 50%)
	if len(best) == 0 || best[0].IP != "5.5.5.1" {
		t.Fatalf("Expected fresh route 5.5.5.1 to rank #1, got: %+v", best)
	}
}

