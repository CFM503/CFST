package main

import (
	"math"
	"sync"
)

// ScoreMode identifies the current scoring preference mode.
type ScoreMode string

const (
	ModeNormal ScoreMode = "normal"
	ModePeak   ScoreMode = "peak"
)

// ComponentWeights configures the percentage contribution of each metric to the score.
type ComponentWeights struct {
	SpeedWeight      float64 `json:"speed_weight"`       // SingleStream/Download speed (0.0 - 1.0)
	StabilityWeight  float64 `json:"stability_weight"`   // Speed stability (0.0 - 1.0)
	PacketLossWeight float64 `json:"packet_loss_weight"` // Packet loss (0.0 - 1.0)
	JitterWeight     float64 `json:"jitter_weight"`      // Latency jitter (0.0 - 1.0)
	LatencyWeight    float64 `json:"latency_weight"`     // TCP Latency / RTT (0.0 - 1.0)
	HandshakeWeight  float64 `json:"handshake_weight"`   // WSS Handshake success (0.0 - 1.0)
	MinSpeedWeight   float64 `json:"min_speed_weight"`   // Floor speed for video streaming (0.0 - 1.0)
	ColoBonus        float64 `json:"colo_bonus"`         // Extra bonus for valid CDN Colo
}

// HorizonWeights configures weights across instant, short-term, and long-term performance.
type HorizonWeights struct {
	InstantWeight   float64 `json:"instant_weight"`    // default: 0.40
	ShortTermWeight float64 `json:"short_term_weight"` // default: 0.35
	LongTermWeight  float64 `json:"long_term_weight"`  // default: 0.25
}

// DefaultNormalWeights returns default weights optimized for normal non-congested hours.
func DefaultNormalWeights() ComponentWeights {
	return ComponentWeights{
		SpeedWeight:      0.25,
		MinSpeedWeight:   0.10,
		StabilityWeight:  0.25,
		PacketLossWeight: 0.15,
		JitterWeight:     0.10,
		LatencyWeight:    0.10,
		HandshakeWeight:  0.05,
		ColoBonus:        5.0,
	}
}

// DefaultPeakWeights returns default weights optimized for peak congested hours (focus on stability & packet loss).
func DefaultPeakWeights() ComponentWeights {
	return ComponentWeights{
		SpeedWeight:      0.15,
		MinSpeedWeight:   0.10,
		StabilityWeight:  0.30,
		PacketLossWeight: 0.25,
		JitterWeight:     0.10,
		LatencyWeight:    0.05,
		HandshakeWeight:  0.05,
		ColoBonus:        3.0,
	}
}

// DefaultHorizonWeights returns default weights across time horizons.
func DefaultHorizonWeights() HorizonWeights {
	return HorizonWeights{
		InstantWeight:   0.40,
		ShortTermWeight: 0.35,
		LongTermWeight:  0.25,
	}
}

// ScoreEngine calculates instant, short-term, long-term, and final route scores.
type ScoreEngine struct {
	mu            sync.RWMutex
	Mode          ScoreMode        `json:"mode"`
	NormalWeights ComponentWeights `json:"normal_weights"`
	PeakWeights   ComponentWeights `json:"peak_weights"`
	Horizons      HorizonWeights   `json:"horizons"`
}

var GlobalScoreEngine = NewScoreEngine(ModeNormal)

func NewScoreEngine(mode ScoreMode) *ScoreEngine {
	if mode == "" {
		mode = ModeNormal
	}
	return &ScoreEngine{
		Mode:          mode,
		NormalWeights: DefaultNormalWeights(),
		PeakWeights:   DefaultPeakWeights(),
		Horizons:      DefaultHorizonWeights(),
	}
}

func (e *ScoreEngine) SetMode(mode ScoreMode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Mode = mode
}

func (e *ScoreEngine) GetMode() ScoreMode {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Mode
}

func (e *ScoreEngine) ActiveWeights() ComponentWeights {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.Mode == ModePeak {
		return e.PeakWeights
	}
	return e.NormalWeights
}

func (e *ScoreEngine) SetWeights(normal, peak ComponentWeights, horizons HorizonWeights) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.NormalWeights = normal
	e.PeakWeights = peak
	e.Horizons = horizons
}

// calcMetricScore calculates a 0-100 composite score from raw metric values.
func (e *ScoreEngine) calcMetricScore(w ComponentWeights, speed, minSpeed, latency, jitter, loss, stability float64, handshake bool, colo string) float64 {
	// 1. Speed score: cap 15 MB/s (~120 Mbps, 4K streaming ceiling)
	scoreSpeed := math.Min(speed/15.0*100.0, 100.0)
	if scoreSpeed < 0 {
		scoreSpeed = 0
	}

	// 2. MinSpeed score: cap 10 MB/s (~80 Mbps buffer floor)
	scoreMinSpeed := math.Min(minSpeed/10.0*100.0, 100.0)
	if scoreMinSpeed < 0 {
		scoreMinSpeed = 0
	}

	// 3. Latency score: base 30ms, penalize higher latency
	scoreLatency := 100.0 - (latency-30.0)*0.5
	if scoreLatency < 0 {
		scoreLatency = 0
	} else if scoreLatency > 100.0 {
		scoreLatency = 100.0
	}

	// 4. Jitter score: >10ms starts steep penalty
	scoreJitter := 100.0 - jitter*2.5
	if scoreJitter < 0 {
		scoreJitter = 0
	} else if scoreJitter > 100.0 {
		scoreJitter = 100.0
	}

	// 5. Packet loss score: 0% loss = 100, 10% loss = 50, >=20% loss = 0
	scoreLoss := 100.0 - loss*500.0
	if scoreLoss < 0 {
		scoreLoss = 0
	} else if scoreLoss > 100.0 {
		scoreLoss = 100.0
	}

	// 6. Stability score (0 - 100)
	scoreStability := stability
	if scoreStability < 0 {
		scoreStability = 0
	} else if scoreStability > 100.0 {
		scoreStability = 100.0
	}

	// 7. Handshake score
	scoreHandshake := 0.0
	if handshake {
		scoreHandshake = 100.0
	}

	total := scoreSpeed*w.SpeedWeight +
		scoreMinSpeed*w.MinSpeedWeight +
		scoreStability*w.StabilityWeight +
		scoreLoss*w.PacketLossWeight +
		scoreJitter*w.JitterWeight +
		scoreLatency*w.LatencyWeight +
		scoreHandshake*w.HandshakeWeight

	if colo != "" && colo != "UNK" && colo != "ERR" && colo != "429" {
		total += w.ColoBonus
	}

	return math.Round(total*10) / 10
}

// EvaluateRoute computes InstantScore, ShortTermScore, LongTermScore, and FinalScore for a route.
func (e *ScoreEngine) EvaluateRoute(m *RouteMetrics, shortEWMA, longEWMA EWMASnapshot, peakHourPenalty float64) {
	w := e.ActiveWeights()

	effectiveSpeed := m.SingleSpeed
	if effectiveSpeed <= 0 {
		effectiveSpeed = m.DownloadSpeed
	}

	// InstantScore from current measurement
	m.InstantScore = e.calcMetricScore(w, effectiveSpeed, m.MinSpeed, m.RTT, m.Jitter, m.PacketLoss, m.Stability, m.HandshakeSuccess, m.Colo)

	// ShortTermScore from short-term EWMA (~5-15 min window)
	shortSpeed := shortEWMA.Speed
	if shortSpeed <= 0 {
		shortSpeed = effectiveSpeed
	}
	shortMinSpeed := m.MinSpeed
	m.ShortTermScore = e.calcMetricScore(w, shortSpeed, shortMinSpeed, shortEWMA.Latency, shortEWMA.Jitter, shortEWMA.Loss, shortEWMA.Stability, m.HandshakeSuccess, m.Colo)

	// LongTermScore from long-term EWMA (~several hours window) with Peak Hour penalty
	longSpeed := longEWMA.Speed
	if longSpeed <= 0 {
		longSpeed = shortSpeed
	}
	rawLongScore := e.calcMetricScore(w, longSpeed, shortMinSpeed, longEWMA.Latency, longEWMA.Jitter, longEWMA.Loss, longEWMA.Stability, m.HandshakeSuccess, m.Colo)
	m.LongTermScore = math.Max(0, rawLongScore-peakHourPenalty)

	// FinalScore = Instant * w_instant + ShortTerm * w_short + LongTerm * w_long
	e.mu.RLock()
	hw := e.Horizons
	e.mu.RUnlock()

	final := m.InstantScore*hw.InstantWeight +
		m.ShortTermScore*hw.ShortTermWeight +
		m.LongTermScore*hw.LongTermWeight

	m.FinalScore = math.Round(final*10) / 10
}
