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
	if wNormal.SpeedWeight != 0.25 || wNormal.StabilityWeight != 0.25 {
		t.Fatalf("unexpected normal weights: %+v", wNormal)
	}

	engine.SetMode(ModePeak)
	if engine.GetMode() != ModePeak {
		t.Fatalf("expected peak mode, got %s", engine.GetMode())
	}

	wPeak := engine.ActiveWeights()
	if wPeak.StabilityWeight != 0.30 || wPeak.PacketLossWeight != 0.25 {
		t.Fatalf("unexpected peak weights: %+v", wPeak)
	}
}

func TestMetricScoreCalculation(t *testing.T) {
	engine := NewScoreEngine(ModeNormal)
	w := engine.ActiveWeights()

	// High quality node: 15MB/s, min 10MB/s, 30ms latency, 0 jitter, 0 loss, 100 stability, valid colo
	scoreGood := engine.calcMetricScore(w, 15.0, 10.0, 30.0, 0.0, 0.0, 100.0, true, "HKG")
	if scoreGood < 95.0 {
		t.Fatalf("expected high quality score > 95, got %.1f", scoreGood)
	}

	// Poor quality node: 1MB/s, min 0.1MB/s, 200ms latency, 20ms jitter, 20% loss, 30 stability, no colo
	scorePoor := engine.calcMetricScore(w, 1.0, 0.1, 200.0, 20.0, 0.20, 30.0, false, "")
	if scorePoor > 30.0 {
		t.Fatalf("expected poor quality score < 30, got %.1f", scorePoor)
	}

	if scoreGood <= scorePoor {
		t.Fatalf("good score (%.1f) should be significantly higher than poor score (%.1f)", scoreGood, scorePoor)
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
		MinSpeed:         8.0,
		Stability:        90.0,
		HandshakeSuccess: true,
	}

	shortEWMA := EWMASnapshot{
		Speed:     8.0,
		Latency:   45.0,
		Loss:      0.0,
		Jitter:    3.0,
		Stability: 85.0,
	}

	longEWMA := EWMASnapshot{
		Speed:     7.0,
		Latency:   50.0,
		Loss:      0.01,
		Jitter:    4.0,
		Stability: 80.0,
	}

	engine.EvaluateRoute(m, shortEWMA, longEWMA, 5.0)

	if m.InstantScore <= 0 || m.ShortTermScore <= 0 || m.LongTermScore <= 0 || m.FinalScore <= 0 {
		t.Fatalf("scores must be positive: instant=%.1f, short=%.1f, long=%.1f, final=%.1f",
			m.InstantScore, m.ShortTermScore, m.LongTermScore, m.FinalScore)
	}

	// FinalScore must reflect: 0.40 * Instant + 0.35 * Short + 0.25 * Long
	expectedFinal := m.InstantScore*0.40 + m.ShortTermScore*0.35 + m.LongTermScore*0.25
	diff := m.FinalScore - expectedFinal
	if diff < -0.2 || diff > 0.2 {
		t.Fatalf("FinalScore %.1f does not match expected weighted sum %.1f", m.FinalScore, expectedFinal)
	}
}
