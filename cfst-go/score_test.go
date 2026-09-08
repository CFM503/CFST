package main

import (
	"testing"
)

func TestScoreWeightsAndModes(t *testing.T) {
	engine := NewScoreEngine(ModeNormal)
	if engine.GetMode() != ModeNormal {
		t.Fatalf("expected normal mode, got %s", engine.GetMode())
	}

	wNormal := engine.ActiveWeights()
	if wNormal.SpeedWeight != 0.20 || wNormal.P10SpeedWeight != 0.20 || wNormal.StabilityWeight != 0.20 {
		t.Fatalf("unexpected normal weights: %+v", wNormal)
	}

	engine.SetMode(ModePeak)
	if engine.GetMode() != ModePeak {
		t.Fatalf("expected peak mode, got %s", engine.GetMode())
	}

	wPeak := engine.ActiveWeights()
	if wPeak.P10SpeedWeight != 0.25 || wPeak.MinSpeedWeight != 0.10 || wPeak.StabilityWeight != 0.25 {
		t.Fatalf("unexpected peak weights: %+v", wPeak)
	}
}

func TestMetricScoreCalculation(t *testing.T) {
	engine := NewScoreEngine(ModeNormal)
	w := engine.ActiveWeights()

	// High quality node: 15MB/s, p10 12MB/s, min 10MB/s, 30ms latency, 0 jitter, 0 loss, 100 stability, valid colo
	scoreGood := engine.calcMetricScore(w, 15.0, 12.0, 10.0, 30.0, 0.0, 0.0, 100.0, true, "HKG")
	if scoreGood < 90.0 {
		t.Fatalf("expected high quality score > 90, got %.1f", scoreGood)
	}

	// Poor quality node: 1MB/s, p10 0.2MB/s, min 0.1MB/s, 200ms latency, 20ms jitter, 20% loss, 30 stability, no colo
	scorePoor := engine.calcMetricScore(w, 1.0, 0.2, 0.1, 200.0, 20.0, 0.20, 30.0, false, "")
	if scorePoor > 35.0 {
		t.Fatalf("expected poor quality score < 35, got %.1f", scorePoor)
	}

	if scoreGood <= scorePoor {
		t.Fatalf("good score (%.1f) should be significantly higher than poor score (%.1f)", scoreGood, scorePoor)
	}
}

func TestStableNodeOutranksPeakStallNode(t *testing.T) {
	engine := NewScoreEngine(ModeNormal)
	w := engine.ActiveWeights()

	// Node A: Peak 100MB/s, but stalls to 0 (P10=2MB/s, Min=0MB/s, Stability=35%, Jitter=15ms, Loss=3%)
	scoreNodeA := engine.calcMetricScore(w, 100.0, 2.0, 0.0, 80.0, 15.0, 0.03, 35.0, true, "SJC")

	// Node B: Rock-solid 55MB/s, never stalls (P10=52MB/s, Min=48MB/s, Stability=96%, Jitter=2ms, Loss=0%)
	scoreNodeB := engine.calcMetricScore(w, 55.0, 52.0, 48.0, 45.0, 2.0, 0.0, 96.0, true, "HKG")

	if scoreNodeB <= scoreNodeA {
		t.Fatalf("Core Requirement Violated: Stable Node B (score %.1f) must outrank peak-stall Node A (score %.1f)",
			scoreNodeB, scoreNodeA)
	}

	// In Peak mode, the advantage of Node B should be even greater
	engine.SetMode(ModePeak)
	wPeak := engine.ActiveWeights()
	peakScoreA := engine.calcMetricScore(wPeak, 100.0, 2.0, 0.0, 80.0, 15.0, 0.03, 35.0, true, "SJC")
	peakScoreB := engine.calcMetricScore(wPeak, 55.0, 52.0, 48.0, 45.0, 2.0, 0.0, 96.0, true, "HKG")

	if peakScoreB <= peakScoreA {
		t.Fatalf("Peak Mode: Stable Node B (score %.1f) must strongly outrank peak-stall Node A (score %.1f)",
			peakScoreB, peakScoreA)
	}
}

func TestEvaluateRouteHorizons(t *testing.T) {
	engine := NewScoreEngine(ModeNormal)

	m := &RouteMetrics{
		IP:               "1.1.1.1",
		Port:             443,
		Colo:             "SJC",
		RTT:              40.0,
		PacketLoss:       0.0,
		Jitter:           2.0,
		SingleSpeed:      10.0,
		P10Speed:         9.0,
		MinSpeed:         8.0,
		Stability:        90.0,
		HandshakeSuccess: true,
		PeakHourScore:    85.0,
		Confidence:       90.0,
	}

	shortEWMA := EWMASnapshot{
		Speed:     8.0,
		P10Speed:  7.5,
		MinSpeed:  7.0,
		Latency:   45.0,
		Loss:      0.0,
		Jitter:    3.0,
		Stability: 85.0,
	}

	longEWMA := EWMASnapshot{
		Speed:     7.0,
		P10Speed:  6.5,
		MinSpeed:  6.0,
		Latency:   50.0,
		Loss:      0.01,
		Jitter:    4.0,
		Stability: 80.0,
	}

	peakHourPenalty := 0.0
	engine.EvaluateRoute(m, shortEWMA, longEWMA, peakHourPenalty)

	if m.InstantScore <= 0 || m.ShortTermScore <= 0 || m.LongTermScore <= 0 || m.FinalScore <= 0 {
		t.Fatalf("scores must be positive: instant=%.1f, short=%.1f, long=%.1f, final=%.1f",
			m.InstantScore, m.ShortTermScore, m.LongTermScore, m.FinalScore)
	}

	// FinalScore in Normal mode: 0.20 * Instant + 0.30 * Short + 0.25 * Long + 0.15 * Peak + 0.10 * Conf
	expectedFinal := m.InstantScore*0.20 + m.ShortTermScore*0.30 + m.LongTermScore*0.25 + m.PeakHourScore*0.15 + m.Confidence*0.10
	diff := m.FinalScore - expectedFinal
	if diff < -0.2 || diff > 0.2 {
		t.Fatalf("FinalScore %.1f does not match expected weighted sum %.1f", m.FinalScore, expectedFinal)
	}
}
