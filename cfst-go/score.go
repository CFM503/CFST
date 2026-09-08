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
	SpeedWeight      float64 `json:"speed_weight"`       // SingleStream/Download speed (0.20)
	P10SpeedWeight   float64 `json:"p10_speed_weight"`   // P10 speed anti-buffering floor (0.20)
	MinSpeedWeight   float64 `json:"min_speed_weight"`   // Floor speed (0.10)
	StabilityWeight  float64 `json:"stability_weight"`   // Speed stability (0.20)
	PacketLossWeight float64 `json:"packet_loss_weight"` // Packet loss (0.10)
	JitterWeight     float64 `json:"jitter_weight"`      // Latency jitter (0.10)
	LatencyWeight    float64 `json:"latency_weight"`     // TCP Latency / RTT (0.05)
	HandshakeWeight  float64 `json:"handshake_weight"`   // WSS Handshake success (0.05)
	ColoBonus        float64 `json:"colo_bonus"`         // Extra bonus for valid CDN Colo
}

// HorizonWeights configures weights across instant, short-term, long-term, peak hour, and confidence.
type HorizonWeights struct {
	InstantWeight    float64 `json:"instant_weight"`
	ShortTermWeight  float64 `json:"short_term_weight"`
	LongTermWeight   float64 `json:"long_term_weight"`
	PeakHourWeight   float64 `json:"peak_hour_weight"`
	ConfidenceWeight float64 `json:"confidence_weight"`
}

// DefaultNormalWeights returns default weights optimized for normal non-congested hours.
func DefaultNormalWeights() ComponentWeights {
	return ComponentWeights{
		SpeedWeight:      0.20,
		P10SpeedWeight:   0.20,
		MinSpeedWeight:   0.10,
		StabilityWeight:  0.20,
		PacketLossWeight: 0.10,
		JitterWeight:     0.10,
		LatencyWeight:    0.05,
		HandshakeWeight:  0.05,
		ColoBonus:        3.0,
	}
}

// DefaultPeakWeights returns default weights optimized for peak congested hours (focus on P10 & stability).
func DefaultPeakWeights() ComponentWeights {
	return ComponentWeights{
		SpeedWeight:      0.15,
		P10SpeedWeight:   0.25,
		MinSpeedWeight:   0.10,
		StabilityWeight:  0.25,
		PacketLossWeight: 0.10,
		JitterWeight:     0.10,
		LatencyWeight:    0.025,
		HandshakeWeight:  0.025,
		ColoBonus:        2.0,
	}
}

// DefaultNormalHorizons returns horizons for normal hours.
func DefaultNormalHorizons() HorizonWeights {
	return HorizonWeights{
		InstantWeight:    0.20,
		ShortTermWeight:  0.30,
		LongTermWeight:   0.25,
		PeakHourWeight:   0.15,
		ConfidenceWeight: 0.10,
	}
}

// DefaultPeakHorizons returns horizons for peak congested hours.
func DefaultPeakHorizons() HorizonWeights {
	return HorizonWeights{
		InstantWeight:    0.10,
		ShortTermWeight:  0.25,
		LongTermWeight:   0.25,
		PeakHourWeight:   0.25,
		ConfidenceWeight: 0.15,
	}
}

// ScoreEngine calculates instant, short-term, long-term, and final route scores.
type ScoreEngine struct {
	mu            sync.RWMutex
	Mode          ScoreMode        `json:"mode"`
	NormalWeights ComponentWeights `json:"normal_weights"`
	PeakWeights   ComponentWeights `json:"peak_weights"`
	NormalHorizon HorizonWeights   `json:"normal_horizons"`
	PeakHorizon   HorizonWeights   `json:"peak_horizons"`
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
		NormalHorizon: DefaultNormalHorizons(),
		PeakHorizon:   DefaultPeakHorizons(),
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

func (e *ScoreEngine) ActiveHorizons() HorizonWeights {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.Mode == ModePeak {
		return e.PeakHorizon
	}
	return e.NormalHorizon
}

func (e *ScoreEngine) SetWeights(normal, peak ComponentWeights, normalH, peakH HorizonWeights) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.NormalWeights = normal
	e.PeakWeights = peak
	e.NormalHorizon = normalH
	e.PeakHorizon = peakH
}

// calcMetricScore calculates a 0-100 composite score from raw metric values.
func (e *ScoreEngine) calcMetricScore(w ComponentWeights, speed, p10Speed, minSpeed, latency, jitter, loss, stability float64, handshake bool, colo string) float64 {
	// 1. Average/Single speed score: cap 15 MB/s (~120 Mbps, 4K ceiling)
	scoreSpeed := math.Min(speed/15.0*100.0, 100.0)
	if scoreSpeed < 0 {
		scoreSpeed = 0
	}

	// 2. P10Speed score: cap 12 MB/s (strong emphasis on anti-buffering floor)
	scoreP10 := math.Min(p10Speed/12.0*100.0, 100.0)
	if scoreP10 < 0 {
		scoreP10 = 0
	}

	// 3. MinSpeed score: cap 8 MB/s (true minimum, 0 if stalled)
	scoreMinSpeed := math.Min(minSpeed/8.0*100.0, 100.0)
	if scoreMinSpeed < 0 {
		scoreMinSpeed = 0
	}

	// 4. Latency score: base 30ms, penalize higher latency
	scoreLatency := 100.0 - (latency-30.0)*0.5
	if scoreLatency < 0 {
		scoreLatency = 0
	} else if scoreLatency > 100.0 {
		scoreLatency = 100.0
	}

	// 5. Jitter score: >10ms starts steep penalty
	scoreJitter := 100.0 - jitter*2.5
	if scoreJitter < 0 {
		scoreJitter = 0
	} else if scoreJitter > 100.0 {
		scoreJitter = 100.0
	}

	// 6. Packet loss score: 0% loss = 100, 10% loss = 50, >=20% loss = 0
	scoreLoss := 100.0 - loss*500.0
	if scoreLoss < 0 {
		scoreLoss = 0
	} else if scoreLoss > 100.0 {
		scoreLoss = 100.0
	}

	// 7. Stability score (0 - 100)
	scoreStability := stability
	if scoreStability < 0 {
		scoreStability = 0
	} else if scoreStability > 100.0 {
		scoreStability = 100.0
	}

	// 8. Handshake score
	scoreHandshake := 0.0
	if handshake {
		scoreHandshake = 100.0
	}

	total := scoreSpeed*w.SpeedWeight +
		scoreP10*w.P10SpeedWeight +
		scoreMinSpeed*w.MinSpeedWeight +
		scoreStability*w.StabilityWeight +
		scoreLoss*w.PacketLossWeight +
		scoreJitter*w.JitterWeight +
		scoreLatency*w.LatencyWeight +
		scoreHandshake*w.HandshakeWeight

	if colo != "" && colo != "UNK" && colo != "ERR" && colo != "429" {
		total += w.ColoBonus
	}

	if total < 0 {
		total = 0
	} else if total > 100.0 {
		total = 100.0
	}
	return math.Round(total*10) / 10
}

// EvaluateRoute computes InstantScore, ShortTermScore, LongTermScore, and FinalScore for a route.
func (e *ScoreEngine) EvaluateRoute(m *RouteMetrics, shortEWMA, longEWMA EWMASnapshot, peakHourPenalty float64) {
	w := e.ActiveWeights()
	hw := e.ActiveHorizons()

	effectiveSpeed := m.SingleSpeed
	if effectiveSpeed <= 0 {
		effectiveSpeed = m.DownloadSpeed
	}
	effectiveP10 := m.P10Speed
	if effectiveP10 <= 0 && effectiveSpeed > 0 {
		effectiveP10 = m.MinSpeed
	}

	// 1. InstantScore from current measurement
	m.InstantScore = e.calcMetricScore(w, effectiveSpeed, effectiveP10, m.MinSpeed, m.RTT, m.Jitter, m.PacketLoss, m.Stability, m.HandshakeSuccess, m.Colo)

	// 2. ShortTermScore from short-term EWMA (~5-15 min window)
	shortSpeed := shortEWMA.Speed
	if shortSpeed <= 0 {
		shortSpeed = effectiveSpeed
	}
	shortP10 := shortEWMA.P10Speed
	if shortP10 <= 0 {
		shortP10 = effectiveP10
	}
	shortMinSpeed := shortEWMA.MinSpeed
	if shortMinSpeed <= 0 {
		shortMinSpeed = m.MinSpeed
	}
	m.ShortTermScore = e.calcMetricScore(w, shortSpeed, shortP10, shortMinSpeed, shortEWMA.Latency, shortEWMA.Jitter, shortEWMA.Loss, shortEWMA.Stability, m.HandshakeSuccess, m.Colo)

	// 3. LongTermScore from long-term EWMA (~several hours)
	longSpeed := longEWMA.Speed
	if longSpeed <= 0 {
		longSpeed = shortSpeed
	}
	longP10 := longEWMA.P10Speed
	if longP10 <= 0 {
		longP10 = shortP10
	}
	longMinSpeed := longEWMA.MinSpeed
	if longMinSpeed <= 0 {
		longMinSpeed = shortMinSpeed
	}
	rawLongScore := e.calcMetricScore(w, longSpeed, longP10, longMinSpeed, longEWMA.Latency, longEWMA.Jitter, longEWMA.Loss, longEWMA.Stability, m.HandshakeSuccess, m.Colo)
	m.LongTermScore = math.Max(0, rawLongScore-peakHourPenalty)

	// 4. PeakHourScore
	peakScore := m.PeakHourScore
	if peakScore <= 0 {
		peakScore = m.LongTermScore
	}

	// 5. Confidence
	conf := m.Confidence
	if conf <= 0 {
		conf = 50.0
	}

	// 6. FinalScore = Instant + ShortTerm + LongTerm + PeakHour + Confidence
	final := m.InstantScore*hw.InstantWeight +
		m.ShortTermScore*hw.ShortTermWeight +
		m.LongTermScore*hw.LongTermWeight +
		peakScore*hw.PeakHourWeight +
		conf*hw.ConfidenceWeight

	if final < 0 {
		final = 0
	} else if final > 100.0 {
		final = 100.0
	}
	m.FinalScore = math.Round(final*10) / 10
}
