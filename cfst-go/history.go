package main

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"sync"
	"time"
)

// MeasurementSample stores one discrete observation for historical analysis.
type MeasurementSample struct {
	Timestamp          time.Time `json:"timestamp"`
	Speed              float64   `json:"speed"`
	P10Speed           float64   `json:"p10_speed"`
	MedianSpeed        float64   `json:"median_speed"`
	MinSpeed           float64   `json:"min_speed"`
	RTT                float64   `json:"rtt"`
	PacketLoss         float64   `json:"packet_loss"`
	Jitter             float64   `json:"jitter"`
	Stability          float64   `json:"stability"`
	StallCount         int       `json:"stall_count"`
	TotalStallDuration float64   `json:"total_stall_duration"`
	ZeroSpeedIntervals int       `json:"zero_speed_intervals"`
	DurationSeconds    float64   `json:"duration_seconds,omitempty"`
	Success            bool      `json:"success"`
}

// WindowStats aggregates metrics over a given sliding window (e.g. 5m, 15m, 1h, 6h, 24h).
type WindowStats struct {
	Duration           string  `json:"duration"`
	Count              int     `json:"count"`
	AvgSpeed           float64 `json:"avg_speed"`
	MedianSpeed        float64 `json:"median_speed"`
	P10Speed           float64 `json:"p10_speed"`
	MinSpeed           float64 `json:"min_speed"`
	AvgRTT             float64 `json:"avg_rtt"`
	P95RTT             float64 `json:"p95_rtt"`
	AvgLoss            float64 `json:"avg_loss"`
	AvgJitter          float64 `json:"avg_jitter"`
	P95Jitter          float64 `json:"p95_jitter"`
	AvgStability       float64 `json:"avg_stability"`
	FailRate           float64 `json:"fail_rate"`
	StallRate          float64 `json:"stall_rate"`
	TotalStallDuration float64 `json:"total_stall_duration"`
	LongestStall       float64 `json:"longest_stall"`
	Confidence         float64 `json:"confidence"` // 0.0 - 100.0
}

// HourStats tracks metrics aggregated for a specific hour of the day (00..23).
type HourStats struct {
	Hour          int       `json:"hour"`
	Count         int       `json:"count"`
	FailCount     int       `json:"fail_count"`
	AvgSpeed      float64   `json:"avg_speed"`
	MedianSpeed   float64   `json:"median_speed"`
	P10Speed      float64   `json:"p10_speed"`
	MinSpeed      float64   `json:"min_speed"`
	AvgRTT        float64   `json:"avg_rtt"`
	AvgLoss       float64   `json:"avg_loss"`
	AvgJitter     float64   `json:"avg_jitter"`
	AvgStability  float64   `json:"avg_stability"`
	StallRate     float64   `json:"stall_rate"`
	FailRate      float64   `json:"fail_rate"`
	PeakHourScore float64   `json:"peak_hour_score"`
	LastUpdated   time.Time `json:"last_updated,omitempty"`
}

// CalcPeakHourScore computes a 0-100 composite quality score for a given hour.
func CalcPeakHourScore(hs *HourStats) float64 {
	if hs == nil || hs.Count == 0 {
		return 50.0 // neutral baseline
	}
	if hs.FailRate >= 1.0 {
		return 0.0
	}

	// Speed/P10 base score (cap 15 MB/s)
	speedScore := math.Min(hs.P10Speed/12.0*100.0, 100.0)
	if speedScore <= 0 && hs.AvgSpeed > 0 {
		speedScore = math.Min(hs.AvgSpeed/15.0*100.0, 100.0)
	}

	// Stability score
	stabilityScore := hs.AvgStability
	if stabilityScore < 0 {
		stabilityScore = 0
	}

	// Jitter score
	jitterScore := math.Max(0, 100.0-hs.AvgJitter*2.5)

	// Loss score
	lossScore := math.Max(0, 100.0-hs.AvgLoss*500.0)

	base := speedScore*0.25 + stabilityScore*0.35 + lossScore*0.20 + jitterScore*0.20

	// Penalize high fail rate and stalls
	penalty := hs.FailRate*40.0 + hs.StallRate*30.0
	score := math.Max(0, base-penalty)
	return math.Round(score*10) / 10
}

// RouteRecord wraps RouteMetrics with its EWMA tracker, sample log, and peak hour matrix.
type RouteRecord struct {
	Metrics   RouteMetrics        `json:"metrics"`
	EWMA      *RouteEWMATracker   `json:"ewma_tracker"`
	PeakHours [24]HourStats       `json:"peak_hours"`
	Samples   []MeasurementSample `json:"samples"`
}

func NewRouteRecord(m RouteMetrics) *RouteRecord {
	r := &RouteRecord{
		Metrics: m,
		EWMA:    NewRouteEWMATracker(0.30, 0.05),
		Samples: make([]MeasurementSample, 0, 120),
	}
	for h := 0; h < 24; h++ {
		r.PeakHours[h] = HourStats{Hour: h, PeakHourScore: 50.0}
	}
	return r
}

// AddSample inserts a new measurement sample, updates PeakHour stats and EWMA, and trims older samples.
func (r *RouteRecord) AddSample(s MeasurementSample) {
	// 1. Append sample
	r.Samples = append(r.Samples, s)

	// Keep samples up to 24 hours (Max 10000 samples to prevent unbounded memory growth while fully covering 24h)
	latestTime := s.Timestamp
	for _, sample := range r.Samples {
		if sample.Timestamp.After(latestTime) {
			latestTime = sample.Timestamp
		}
	}
	cutoff := latestTime.Add(-24 * time.Hour)
	startIdx := 0
	for startIdx < len(r.Samples) && r.Samples[startIdx].Timestamp.Before(cutoff) {
		startIdx++
	}
	if startIdx > 0 {
		r.Samples = r.Samples[startIdx:]
	}
	if len(r.Samples) > 10000 {
		r.Samples = r.Samples[len(r.Samples)-10000:]
	}

	// 2. Update EWMA
	sampleDuration := s.DurationSeconds
	if sampleDuration <= 0 {
		sampleDuration = 10.0 // legacy snapshot fallback
	}

	if s.Success {
		stallRate := 0.0
		if s.TotalStallDuration > 0 {
			stallRate = s.TotalStallDuration / sampleDuration
			if stallRate > 1.0 {
				stallRate = 1.0
			}
		}
		r.EWMA.Record(s.Speed, s.P10Speed, s.MinSpeed, s.RTT, s.PacketLoss, s.Jitter, s.Stability, stallRate, s.Timestamp)
	} else {
		r.EWMA.RecordFailure(s.Timestamp)
	}

	// 3. Update PeakHour stats (0..23) with time-based decay
	h := s.Timestamp.Hour()
	hs := &r.PeakHours[h]

	isInitial := (hs.Count == 0 && hs.AvgSpeed == 0)

	// If hs was updated on a previous day/cycle (>12h ago), apply exponential decay to historical counts
	if !hs.LastUpdated.IsZero() && s.Timestamp.Sub(hs.LastUpdated) > 12*time.Hour {
		days := math.Max(1.0, s.Timestamp.Sub(hs.LastUpdated).Hours()/24.0)
		decay := math.Pow(0.60, days)
		hs.Count = int(float64(hs.Count) * decay)
		if hs.Count < 1 {
			hs.Count = 1
		}
		hs.FailCount = int(float64(hs.FailCount) * decay)
	}
	hs.LastUpdated = s.Timestamp
	hs.Count++

	if !s.Success {
		hs.FailCount++
	} else {
		// Alpha-weighted EWMA update so recent peak hour quality has stronger weight
		alpha := 0.35
		if isInitial {
			hs.AvgSpeed = s.Speed
			hs.MedianSpeed = s.MedianSpeed
			hs.P10Speed = s.P10Speed
			hs.MinSpeed = s.MinSpeed
			hs.AvgRTT = s.RTT
			hs.AvgLoss = s.PacketLoss
			hs.AvgJitter = s.Jitter
			hs.AvgStability = s.Stability
			if s.TotalStallDuration > 0 {
				hs.StallRate = s.TotalStallDuration / sampleDuration
			}
		} else {
			hs.AvgSpeed = hs.AvgSpeed*(1-alpha) + s.Speed*alpha
			if s.MedianSpeed > 0 {
				hs.MedianSpeed = hs.MedianSpeed*(1-alpha) + s.MedianSpeed*alpha
			}
			if s.P10Speed > 0 {
				hs.P10Speed = hs.P10Speed*(1-alpha) + s.P10Speed*alpha
			}
			if s.MinSpeed < hs.MinSpeed || hs.MinSpeed <= 0 {
				hs.MinSpeed = s.MinSpeed
			} else {
				hs.MinSpeed = hs.MinSpeed*(1-alpha) + s.MinSpeed*alpha
			}
			hs.AvgRTT = hs.AvgRTT*(1-alpha) + s.RTT*alpha
			hs.AvgLoss = hs.AvgLoss*(1-alpha) + s.PacketLoss*alpha
			hs.AvgJitter = hs.AvgJitter*(1-alpha) + s.Jitter*alpha
			hs.AvgStability = hs.AvgStability*(1-alpha) + s.Stability*alpha
			sampleStallRate := s.TotalStallDuration / sampleDuration
			hs.StallRate = hs.StallRate*(1-alpha) + sampleStallRate*alpha
		}
	}
	if hs.Count > 0 {
		hs.FailRate = float64(hs.FailCount) / float64(hs.Count)
	}
	hs.PeakHourScore = CalcPeakHourScore(hs)
}

// CalcConfidence computes a 0-100 confidence score based on:
// 1. Observation duration (span between earliest and latest sample)
// 2. Sample count
// 3. Success rate
// 4. Peak hour coverage
// 5. Recency
func (r *RouteRecord) CalcConfidence(now time.Time) float64 {
	if len(r.Samples) == 0 {
		return 20.0 // neutral starting baseline
	}

	firstTime := r.Samples[0].Timestamp
	lastTime := r.Samples[len(r.Samples)-1].Timestamp
	span := lastTime.Sub(firstTime)
	if span < 0 {
		span = 0
	}

	total := len(r.Samples)
	var validCount int
	hasPeakHour := false
	for _, s := range r.Samples {
		if s.Success {
			validCount++
		}
		h := s.Timestamp.Hour()
		if h >= 20 && h <= 22 {
			hasPeakHour = true
		}
	}

	// 1. Duration score (0 to 50 points): requires observation time to climb
	var durationScore float64
	spanMins := span.Minutes()
	if spanMins < 2 {
		durationScore = 10.0
	} else if spanMins < 5 {
		durationScore = 15.0
	} else if spanMins < 15 {
		durationScore = 25.0
	} else if spanMins < 30 {
		durationScore = 35.0
	} else if spanMins < 60 {
		durationScore = 40.0
	} else if spanMins < 180 {
		durationScore = 45.0
	} else {
		durationScore = 50.0
	}

	// 2. Sample count score (0 to 30 points)
	countScore := math.Min(30.0, float64(total)/20.0*30.0)

	// 3. Peak hour coverage bonus (0 to 10 points)
	peakBonus := 0.0
	if hasPeakHour && spanMins >= 15 {
		peakBonus = 10.0
	}

	// 4. Stability / Success bonus (0 to 10 points)
	successRate := float64(validCount) / float64(total)
	successBonus := successRate * 10.0

	rawConfidence := durationScore + countScore + peakBonus + successBonus

	// 5. Recency penalty (if last tested was long ago)
	timeSinceLast := now.Sub(lastTime)
	if timeSinceLast > 10*time.Minute {
		overMins := timeSinceLast.Minutes() - 10.0
		decay := math.Max(0.50, 1.0-(overMins/60.0)*0.50)
		rawConfidence *= decay
	}

	if rawConfidence < 20.0 {
		rawConfidence = 20.0
	} else if rawConfidence > 100.0 {
		rawConfidence = 100.0
	}
	return math.Round(rawConfidence*10) / 10
}

// CalcWindowStats aggregates samples within the specified duration before now.
func (r *RouteRecord) CalcWindowStats(d time.Duration, now time.Time) WindowStats {
	cutoff := now.Add(-d)
	var sumSpeed, sumRTT, sumLoss, sumJitter, sumStable float64
	var totalStallDuration, longestStall float64
	var minSpd float64 = math.MaxFloat64
	var validCount, failCount int
	var speeds, rtts, jitters []float64

	for i := len(r.Samples) - 1; i >= 0; i-- {
		s := r.Samples[i]
		if s.Timestamp.Before(cutoff) {
			break
		}
		if !s.Success {
			failCount++
			continue
		}
		validCount++
		sumSpeed += s.Speed
		speeds = append(speeds, s.Speed)
		if s.MinSpeed < minSpd {
			minSpd = s.MinSpeed
		}
		sumRTT += s.RTT
		rtts = append(rtts, s.RTT)
		sumLoss += s.PacketLoss
		sumJitter += s.Jitter
		jitters = append(jitters, s.Jitter)
		sumStable += s.Stability

		totalStallDuration += s.TotalStallDuration
		if s.TotalStallDuration > longestStall {
			longestStall = s.TotalStallDuration
		}
	}

	total := validCount + failCount
	ws := WindowStats{Duration: d.String(), Count: total}
	if total == 0 {
		ws.Confidence = 0.0
		return ws
	}

	ws.Confidence = r.CalcConfidence(now)

	ws.FailRate = float64(failCount) / float64(total)
	if validCount > 0 {
		vc := float64(validCount)
		ws.AvgSpeed = math.Round((sumSpeed/vc)*10) / 10
		if minSpd != math.MaxFloat64 {
			ws.MinSpeed = math.Round(minSpd*10) / 10
		}
		ws.AvgRTT = math.Round((sumRTT/vc)*10) / 10
		ws.AvgLoss = math.Round((sumLoss/vc)*1000) / 1000
		ws.AvgJitter = math.Round((sumJitter/vc)*10) / 10
		ws.AvgStability = math.Round((sumStable/vc)*10) / 10
		ws.TotalStallDuration = math.Round(totalStallDuration*10) / 10
		ws.LongestStall = math.Round(longestStall*10) / 10
		if d.Seconds() > 0 {
			ws.StallRate = math.Min(1.0, totalStallDuration/d.Seconds())
		}

		sort.Float64s(speeds)
		ws.P10Speed = math.Round(calcPercentile(speeds, 0.10)*10) / 10
		ws.MedianSpeed = math.Round(calcPercentile(speeds, 0.50)*10) / 10

		sort.Float64s(rtts)
		ws.P95RTT = math.Round(calcPercentile(rtts, 0.95)*10) / 10

		sort.Float64s(jitters)
		ws.P95Jitter = math.Round(calcPercentile(jitters, 0.95)*10) / 10
	}
	return ws
}

// PeakHourPenalty returns penalty points for this route at the given hour based on historical failure/speed drops.
func (r *RouteRecord) PeakHourPenalty(h int) float64 {
	hs := r.PeakHours[h]
	if hs.Count < 3 {
		return 0.0
	}
	penalty := 0.0
	if hs.FailRate > 0.20 {
		penalty += hs.FailRate * 30.0
	}
	if hs.AvgLoss > 0.10 {
		penalty += hs.AvgLoss * 40.0
	}
	if hs.AvgJitter > 20.0 {
		penalty += (hs.AvgJitter - 20.0) * 0.5
	}
	if hs.StallRate > 0.05 {
		penalty += hs.StallRate * 30.0
	}
	return math.Min(penalty, 40.0)
}

// RouteStore manages all active, standby, candidate, and failed routes in memory.
type RouteStore struct {
	mu     sync.RWMutex
	routes map[string]*RouteRecord // keyed by route IP or ID
}

var GlobalRouteStore = NewRouteStore()

func NewRouteStore() *RouteStore {
	return &RouteStore{
		routes: make(map[string]*RouteRecord),
	}
}

func (s *RouteStore) UpsertRoute(m RouteMetrics) *RouteRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.routes[m.IP]
	if !exists {
		rec = NewRouteRecord(m)
		s.routes[m.IP] = rec
		s.routes[m.ID] = rec
		return rec
	}

	// Update mutable metric fields
	rec.Metrics.IP = m.IP
	rec.Metrics.Port = m.Port
	rec.Metrics.Colo = m.Colo
	if m.Tier != "" {
		rec.Metrics.Tier = m.Tier
	}
	rec.Metrics.Health = m.Health
	rec.Metrics.StabilityGrade = m.StabilityGrade
	rec.Metrics.Recommendation = m.Recommendation
	rec.Metrics.RecommendationReasons = m.RecommendationReasons
	rec.Metrics.RTT = m.RTT
	rec.Metrics.PacketLoss = m.PacketLoss
	rec.Metrics.Jitter = m.Jitter
	rec.Metrics.DownloadSpeed = m.DownloadSpeed
	rec.Metrics.SingleSpeed = m.SingleSpeed
	rec.Metrics.P10Speed = m.P10Speed
	rec.Metrics.P25Speed = m.P25Speed
	rec.Metrics.MedianSpeed = m.MedianSpeed
	rec.Metrics.MinSpeed = m.MinSpeed
	rec.Metrics.MaxSpeed = m.MaxSpeed
	rec.Metrics.StdDev = m.StdDev
	rec.Metrics.CV = m.CV
	rec.Metrics.Stability = m.Stability
	rec.Metrics.LoadLatency = m.LoadLatency
	rec.Metrics.HandshakeSuccess = m.HandshakeSuccess
	rec.Metrics.GOWAYWSSCompatible = m.GOWAYWSSCompatible
	rec.Metrics.GOWAYWSSLatency = m.GOWAYWSSLatency
	rec.Metrics.GOWAYWSSErrorStage = m.GOWAYWSSErrorStage
	rec.Metrics.GOWAYWSSHTTPStatus = m.GOWAYWSSHTTPStatus
	rec.Metrics.GOWAYWSSErrorMessage = m.GOWAYWSSErrorMessage
	rec.Metrics.GOWAYWSSSNISent = m.GOWAYWSSSNISent
	rec.Metrics.GOWAYWSSHostSent = m.GOWAYWSSHostSent
	rec.Metrics.GOWAYWSSPathSent = m.GOWAYWSSPathSent
	rec.Metrics.ZeroSpeedIntervals = m.ZeroSpeedIntervals
	rec.Metrics.StallCount = m.StallCount
	rec.Metrics.TotalStallDuration = m.TotalStallDuration
	rec.Metrics.LongestStallDuration = m.LongestStallDuration
	rec.Metrics.StallRate = m.StallRate
	rec.Metrics.BaselineP10 = m.BaselineP10
	rec.Metrics.SpeedDropPercent = m.SpeedDropPercent
	rec.Metrics.Confidence = m.Confidence
	rec.Metrics.PeakHourScore = m.PeakHourScore
	rec.Metrics.InstantScore = m.InstantScore
	rec.Metrics.ShortTermScore = m.ShortTermScore
	rec.Metrics.LongTermScore = m.LongTermScore
	rec.Metrics.FinalScore = m.FinalScore
	rec.Metrics.ConsecutiveFails = m.ConsecutiveFails
	rec.Metrics.ConsecutiveSuccess = m.ConsecutiveSuccess
	rec.Metrics.ConsecutiveDegraded = m.ConsecutiveDegraded
	rec.Metrics.LastTested = m.LastTested
	rec.Metrics.Timestamp = m.Timestamp
	rec.Metrics.IsStale = m.IsStale
	rec.Metrics.ObservationDuration = m.ObservationDuration
	rec.Metrics.LastSuccess = m.LastSuccess

	return rec
}

func (s *RouteStore) RecordProbeResult(m RouteMetrics, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.routes[m.IP]
	if !exists {
		rec = NewRouteRecord(m)
		s.routes[m.IP] = rec
		s.routes[m.ID] = rec
	}

	sample := MeasurementSample{
		Timestamp:          m.LastTested,
		Speed:              m.SingleSpeed,
		P10Speed:           m.P10Speed,
		MedianSpeed:        m.MedianSpeed,
		MinSpeed:           m.MinSpeed,
		RTT:                m.RTT,
		PacketLoss:         m.PacketLoss,
		Jitter:             m.Jitter,
		Stability:          m.Stability,
		StallCount:         m.StallCount,
		TotalStallDuration: m.TotalStallDuration,
		ZeroSpeedIntervals: m.ZeroSpeedIntervals,
		DurationSeconds:    m.DurationSeconds,
		Success:            success,
	}
	if sample.Speed <= 0 {
		sample.Speed = m.DownloadSpeed
	}
	if sample.P10Speed <= 0 && sample.Speed > 0 {
		sample.P10Speed = m.MinSpeed
	}
	if sample.MedianSpeed <= 0 && sample.Speed > 0 {
		sample.MedianSpeed = sample.Speed
	}
	if sample.Timestamp.IsZero() {
		sample.Timestamp = time.Now()
	}

	// 1. Capture baseline P10 BEFORE adding current sample to EWMA / History
	// This ensures sudden speed drops are accurately measured rather than immediately smoothed out.
	baselineP10 := rec.EWMA.LongSnapshot().P10Speed
	if baselineP10 <= 0 {
		baselineP10 = rec.EWMA.LongSnapshot().Speed
	}
	m.BaselineP10 = baselineP10

	// Pre-calculate SpeedDropPercent against uncorrupted baseline
	effSpeed := sample.P10Speed
	if effSpeed <= 0 && sample.MinSpeed > 0 {
		effSpeed = sample.MinSpeed
	}
	if effSpeed <= 0 {
		effSpeed = sample.Speed
	}
	if baselineP10 > 2.0 && effSpeed >= 0 {
		drop := (1.0 - (effSpeed / baselineP10)) * 100.0
		if drop > 0 {
			m.SpeedDropPercent = math.Round(drop*10) / 10
		} else {
			m.SpeedDropPercent = 0
		}
	} else {
		m.SpeedDropPercent = 0
	}

	// 2. Update health status using uncorrupted baseline
	if success {
		m.LastSuccess = sample.Timestamp
	} else if !rec.Metrics.LastSuccess.IsZero() {
		m.LastSuccess = rec.Metrics.LastSuccess
	}
	m.UpdateHealth(success, baselineP10)

	// 3. Now add sample to history and update EWMA
	rec.AddSample(sample)

	// Update EWMA snapshot
	snap := rec.EWMA.ShortSnapshot()
	m.EWMA = &snap

	// Evaluate confidence based on time duration + count + success rate + recency
	m.Confidence = rec.CalcConfidence(sample.Timestamp)
	if len(rec.Samples) > 0 {
		m.ObservationDuration = rec.Samples[len(rec.Samples)-1].Timestamp.Sub(rec.Samples[0].Timestamp).Seconds()
	}

	// Stale route check
	now := sample.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	if !m.LastTested.IsZero() {
		if m.Tier == TierActive && now.Sub(m.LastTested) > 5*time.Minute {
			m.IsStale = true
		} else if m.Tier != TierActive && now.Sub(m.LastTested) > 15*time.Minute {
			m.IsStale = true
		} else {
			m.IsStale = false
		}
	}
	if m.IsStale {
		m.Confidence *= 0.50
	}

	// Update current hour's PeakHourScore
	h := sample.Timestamp.Hour()
	m.PeakHourScore = rec.PeakHours[h].PeakHourScore

	// Evaluate multi-horizon scores
	penalty := rec.PeakHourPenalty(h)
	GlobalScoreEngine.EvaluateRoute(&m, snap, rec.EWMA.LongSnapshot(), penalty)
	if m.IsStale {
		m.FinalScore *= 0.50
	}

	// Assign grade and recommendation
	m.StabilityGrade = m.DetermineStabilityGrade()
	recType, reasons := m.GenerateRecommendation()
	m.Recommendation = recType
	m.RecommendationReasons = reasons

	// Commit updated metrics into record
	rec.Metrics = m

	// Automatically evaluate candidate promotion and degraded route demotion
	s.evaluateTierTransitionsLocked(sample.Timestamp)
}

func (s *RouteStore) Get(idOrIP string) (*RouteRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.routes[idOrIP]
	return rec, ok
}

// GetMetrics returns a thread-safe snapshot copy of RouteMetrics, preventing pointer leakage.
func (s *RouteStore) GetMetrics(idOrIP string) (RouteMetrics, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.routes[idOrIP]
	if !ok || rec == nil {
		return RouteMetrics{}, false
	}
	return rec.Metrics, true
}

// GetSamples returns a thread-safe copy of historical measurement samples.
func (s *RouteStore) GetSamples(idOrIP string) []MeasurementSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.routes[idOrIP]
	if !ok || rec == nil {
		return nil
	}
	samples := make([]MeasurementSample, len(rec.Samples))
	copy(samples, rec.Samples)
	return samples
}

// UpdateRouteColo safely updates the Colo for an existing route under lock if it was previously empty.
func (s *RouteStore) UpdateRouteColo(ip, colo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.routes[ip]
	if !ok || rec == nil {
		return false
	}
	if rec.Metrics.Colo == "" && colo != "" {
		rec.Metrics.Colo = colo
		return true
	}
	return false
}

// getAllLocked returns all unique RouteMetrics without acquiring locks (call while holding mu).
func (s *RouteStore) getAllLocked() []RouteMetrics {
	seen := make(map[string]bool)
	var list []RouteMetrics
	now := time.Now()
	for _, rec := range s.routes {
		if !seen[rec.Metrics.IP] {
			seen[rec.Metrics.IP] = true
			m := rec.Metrics
			if !m.LastTested.IsZero() {
				if m.Tier == TierActive && now.Sub(m.LastTested) > 5*time.Minute {
					m.IsStale = true
				} else if m.Tier != TierActive && now.Sub(m.LastTested) > 15*time.Minute {
					m.IsStale = true
				}
			}
			list = append(list, m)
		}
	}
	return list
}

func (s *RouteStore) GetAll() []RouteMetrics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getAllLocked()
}

// GetBest selects routes optimizing for stability > peak speed, with multi-level tie-breaking.
func (s *RouteStore) GetBest(limit int, colo string) []RouteMetrics {
	s.mu.RLock()
	all := s.getAllLocked()
	s.mu.RUnlock()

	mode := GlobalScoreEngine.GetMode()
	now := time.Now()
	curHour := now.Hour()
	isPeakTime := (mode == ModePeak) || (curHour >= 19 && curHour <= 23)

	type candidateScore struct {
		m              RouteMetrics
		effectiveScore float64
	}

	var scored []candidateScore
	for _, m := range all {
		if colo != "" && m.Colo != colo {
			continue
		}

		// Stale route protection:
		// If untested for > 60 minutes, exclude from GetBest() completely.
		timeSinceTest := now.Sub(m.LastTested)
		if !m.LastTested.IsZero() && timeSinceTest > 60*time.Minute {
			continue
		}

		// 1. Exclude FAILED and FAILING routes
		if m.Health == HealthFailed || m.Health == HealthFailing {
			continue
		}

		effective := m.FinalScore

		// Stale route penalization: untested > 5m for active or > 15m for others
		if m.IsStale || (!m.LastTested.IsZero() && ((m.Tier == TierActive && timeSinceTest > 5*time.Minute) || (m.Tier != TierActive && timeSinceTest > 15*time.Minute))) {
			m.IsStale = true
			effective *= 0.50
			m.Confidence *= 0.50
		}

		// 2. RECOVERING routes downweighted by 30%
		if m.Health == HealthRecovering {
			effective *= 0.70
		}

		// 3. DEGRADED routes downweighted by 25%
		if m.Health == HealthDegraded {
			effective *= 0.75
		}

		// 4. Low confidence penalty (ensure unverified routes don't outrank proven nodes)
		conf := m.Confidence
		if conf < 10.0 {
			conf = 10.0
		}
		effective *= (0.50 + 0.50*(conf/100.0))

		// 5. Peak Hour mode prioritization
		if isPeakTime && m.PeakHourScore > 0 {
			effective = effective*0.65 + m.PeakHourScore*0.35
		}

		scored = append(scored, candidateScore{m: m, effectiveScore: effective})
	}

	// Multi-level sorting:
	// 1. EffectiveScore descending
	// 2. Tie-break: higher P10Speed
	// 3. Tie-break: lower Jitter
	// 4. Tie-break: lower PacketLoss
	// 5. Tie-break: lower RTT
	sort.Slice(scored, func(i, j int) bool {
		diff := scored[i].effectiveScore - scored[j].effectiveScore
		if math.Abs(diff) > 0.5 {
			return scored[i].effectiveScore > scored[j].effectiveScore
		}
		// Tie-break 1: P10 Speed
		if math.Abs(scored[i].m.P10Speed-scored[j].m.P10Speed) > 0.2 {
			return scored[i].m.P10Speed > scored[j].m.P10Speed
		}
		// Tie-break 2: Jitter
		if math.Abs(scored[i].m.Jitter-scored[j].m.Jitter) > 1.0 {
			return scored[i].m.Jitter < scored[j].m.Jitter
		}
		// Tie-break 3: Packet Loss
		if math.Abs(scored[i].m.PacketLoss-scored[j].m.PacketLoss) > 0.01 {
			return scored[i].m.PacketLoss < scored[j].m.PacketLoss
		}
		// Tie-break 4: TCPLatency / RTT
		return scored[i].m.RTT < scored[j].m.RTT
	})

	var result []RouteMetrics
	for _, sc := range scored {
		result = append(result, sc.m)
	}

	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result
}

func (s *RouteStore) SetTier(idOrIP string, tier RouteTier) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.routes[idOrIP]
	if !ok {
		return false
	}
	rec.Metrics.Tier = tier
	return true
}

func (s *RouteStore) GetHistoryWindows(idOrIP string) map[string]WindowStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.routes[idOrIP]
	if !ok {
		return nil
	}
	now := time.Now()
	return map[string]WindowStats{
		"5m":  rec.CalcWindowStats(5*time.Minute, now),
		"15m": rec.CalcWindowStats(15*time.Minute, now),
		"1h":  rec.CalcWindowStats(1*time.Hour, now),
		"6h":  rec.CalcWindowStats(6*time.Hour, now),
		"24h": rec.CalcWindowStats(24*time.Hour, now),
	}
}

func (s *RouteStore) GetPeakHours(idOrIP string) [24]HourStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.routes[idOrIP]
	if !ok {
		return [24]HourStats{}
	}
	return rec.PeakHours
}

// DeleteRoute safely removes a route by its IP or ID from the RouteStore.
func (s *RouteStore) DeleteRoute(idOrIP string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.routes[idOrIP]
	if !ok {
		return false
	}
	delete(s.routes, rec.Metrics.IP)
	delete(s.routes, rec.Metrics.ID)
	return true
}

// EvaluateTierTransitions evaluates promotion, demotion, and pruning across all managed routes.
func (s *RouteStore) EvaluateTierTransitions(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evaluateTierTransitionsLocked(now)
}

func (s *RouteStore) evaluateTierTransitionsLocked(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}

	seen := make(map[string]bool)
	var records []*RouteRecord
	for _, rec := range s.routes {
		if rec != nil && !seen[rec.Metrics.IP] {
			seen[rec.Metrics.IP] = true
			records = append(records, rec)
		}
	}

	// 1. Long-term failed route pruning and initial tier categorization:
	var activeRec *RouteRecord
	var standbyRecs []*RouteRecord
	var candidateRecs []*RouteRecord

	for _, rec := range records {
		m := &rec.Metrics
		// Prune if Health == HealthFailed, ConsecutiveFails >= 10, and untested for > 2 hours
		if m.Health == HealthFailed && m.ConsecutiveFails >= 10 && !m.LastTested.IsZero() && now.Sub(m.LastTested) > 2*time.Hour {
			delete(s.routes, m.IP)
			delete(s.routes, m.ID)
			continue
		}

		switch m.Tier {
		case TierActive:
			activeRec = rec
		case TierStandby:
			standbyRecs = append(standbyRecs, rec)
		case TierCandidate:
			candidateRecs = append(candidateRecs, rec)
		}
	}

	// Ensure each route transitions at most once per evaluation cycle
	transitioned := make(map[string]bool)

	// 2. Demotion: Active -> Standby
	if activeRec != nil {
		m := &activeRec.Metrics
		if m.Health == HealthDegraded || m.Health == HealthFailing || m.Health == HealthFailed || m.SpeedDropPercent >= 35.0 || m.ConsecutiveFails >= 2 {
			m.Tier = TierStandby
			transitioned[m.IP] = true
			standbyRecs = append(standbyRecs, activeRec)
			activeRec = nil
		}
	}

	// 3. Demotion: Standby -> Candidate
	var remainingStandbys []*RouteRecord
	for _, rec := range standbyRecs {
		if transitioned[rec.Metrics.IP] {
			remainingStandbys = append(remainingStandbys, rec)
			continue
		}
		m := &rec.Metrics
		if m.Health == HealthFailing || m.Health == HealthFailed || (m.ObservationDuration >= 300.0 && m.FinalScore < 50.0 && m.FinalScore > 0) || m.ConsecutiveFails >= 3 {
			m.Tier = TierCandidate
			transitioned[m.IP] = true
			candidateRecs = append(candidateRecs, rec)
		} else {
			remainingStandbys = append(remainingStandbys, rec)
		}
	}
	standbyRecs = remainingStandbys

	// 4. Demotion: Candidate -> Failed
	var remainingCandidates []*RouteRecord
	for _, rec := range candidateRecs {
		if transitioned[rec.Metrics.IP] {
			remainingCandidates = append(remainingCandidates, rec)
			continue
		}
		m := &rec.Metrics
		if m.Health == HealthFailed || m.ConsecutiveFails >= 5 {
			m.Tier = TierFailed
			transitioned[m.IP] = true
		} else {
			remainingCandidates = append(remainingCandidates, rec)
		}
	}
	candidateRecs = remainingCandidates

	// 5. Promotion: Candidate -> Standby
	// Requires:
	// - ObservationDuration >= 300s (5 min)
	// - len(Samples) >= 3
	// - Confidence >= 35.0
	// - Health == HealthHealthy
	// - FinalScore >= 70.0
	// - PacketLoss <= 0.05
	// - Jitter <= 25.0
	// - StallCount == 0
	for _, rec := range candidateRecs {
		if transitioned[rec.Metrics.IP] {
			continue
		}
		m := &rec.Metrics
		if m.ObservationDuration >= 300.0 && len(rec.Samples) >= 3 && m.Confidence >= 35.0 &&
			m.Health == HealthHealthy && m.FinalScore >= 70.0 && m.PacketLoss <= 0.05 &&
			m.Jitter <= 25.0 && m.StallCount == 0 {
			m.Tier = TierStandby
			transitioned[m.IP] = true
			standbyRecs = append(standbyRecs, rec)
		}
	}

	// 6. Promotion: Standby -> Active
	// Eligible standby requires:
	// - Health == HealthHealthy
	// - Confidence >= 50.0
	// - FinalScore >= 70.0
	// - PacketLoss <= 0.02
	// - StallCount == 0
	if activeRec == nil {
		var bestStandby *RouteRecord
		for _, rec := range standbyRecs {
			if transitioned[rec.Metrics.IP] {
				continue
			}
			m := &rec.Metrics
			if m.Health == HealthHealthy && m.Confidence >= 50.0 && m.FinalScore >= 70.0 && m.PacketLoss <= 0.02 && m.StallCount == 0 {
				if bestStandby == nil || m.FinalScore > bestStandby.Metrics.FinalScore {
					bestStandby = rec
				}
			}
		}
		if bestStandby != nil {
			bestStandby.Metrics.Tier = TierActive
			activeRec = bestStandby
		}
	} else {
		// Active exists: only replace if Standby significantly and consistently beats Active over long term
		// FinalScore >= active.FinalScore + 5.0 && Confidence >= 60.0 && P10Speed >= active.P10Speed && ObservationDuration >= 900s
		var bestChallenger *RouteRecord
		for _, rec := range standbyRecs {
			if transitioned[rec.Metrics.IP] {
				continue
			}
			m := &rec.Metrics
			if m.Health == HealthHealthy && m.Confidence >= 60.0 && m.ObservationDuration >= 900.0 &&
				m.FinalScore >= activeRec.Metrics.FinalScore+5.0 && m.P10Speed >= activeRec.Metrics.P10Speed &&
				m.PacketLoss <= 0.02 && m.StallCount == 0 {
				if bestChallenger == nil || m.FinalScore > bestChallenger.Metrics.FinalScore {
					bestChallenger = rec
				}
			}
		}
		if bestChallenger != nil {
			activeRec.Metrics.Tier = TierStandby
			bestChallenger.Metrics.Tier = TierActive
			activeRec = bestChallenger
		}
	}
}

// SnapshotContainer stores full state for robust serialization.
type SnapshotContainer struct {
	Version   string                  `json:"version"`
	Timestamp time.Time               `json:"timestamp"`
	Routes    map[string]*RouteRecord `json:"routes"`
}

// Clone returns a deep copy of the record safe to marshal outside the store lock.
// Caller must hold at least s.mu.RLock() so concurrent RecordProbeResult/Upsert
// (which hold s.mu.Lock()) cannot mutate rec while it is being copied.
func (r *RouteRecord) Clone() *RouteRecord {
	if r == nil {
		return nil
	}
	cp := &RouteRecord{
		Metrics:   r.Metrics,
		PeakHours: r.PeakHours,
		EWMA:      r.EWMA.Clone(),
	}
	if r.Samples != nil {
		cp.Samples = make([]MeasurementSample, len(r.Samples))
		copy(cp.Samples, r.Samples)
	}
	if r.Metrics.RecommendationReasons != nil {
		reasons := make([]string, len(r.Metrics.RecommendationReasons))
		copy(reasons, r.Metrics.RecommendationReasons)
		cp.Metrics.RecommendationReasons = reasons
	}
	if r.Metrics.EWMA != nil {
		snap := *r.Metrics.EWMA
		cp.Metrics.EWMA = &snap
	}
	return cp
}

// SaveSnapshot serializes full state to a JSON file atomically (Windows safe).
func (s *RouteStore) SaveSnapshot(path string) error {
	// Deep-copy under RLock so json.Marshal below never reads live records
	// concurrently mutated by RecordProbeResult/AddSample/Upsert.
	s.mu.RLock()
	uniqueRecords := make(map[string]*RouteRecord)
	for ip, rec := range s.routes {
		if rec != nil && rec.Metrics.IP == ip {
			uniqueRecords[ip] = rec.Clone()
		}
	}
	s.mu.RUnlock()

	container := SnapshotContainer{
		Version:   "2.1.8",
		Timestamp: time.Now(),
		Routes:    uniqueRecords,
	}

	data, err := json.MarshalIndent(container, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}

	// Atomic replace: on Windows remove target first if needed
	_ = os.Remove(path)
	return os.Rename(tmpPath, path)
}

// LoadSnapshot restores routes from a JSON snapshot file with version tolerance.
func (s *RouteStore) LoadSnapshot(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// 1. Try modern v2.1.0 SnapshotContainer
	var container SnapshotContainer
	if err := json.Unmarshal(data, &container); err == nil && len(container.Routes) > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		for ip, rec := range container.Routes {
			if rec != nil {
				s.routes[ip] = rec
				s.routes[rec.Metrics.ID] = rec
			}
		}
		return nil
	}

	// 2. Try legacy v2.0 []RouteMetrics
	var legacyList []RouteMetrics
	if err := json.Unmarshal(data, &legacyList); err == nil {
		for _, m := range legacyList {
			s.UpsertRoute(m)
		}
		return nil
	}

	return err
}
