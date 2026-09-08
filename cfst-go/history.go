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
	Hour          int     `json:"hour"`
	Count         int     `json:"count"`
	FailCount     int     `json:"fail_count"`
	AvgSpeed      float64 `json:"avg_speed"`
	MedianSpeed   float64 `json:"median_speed"`
	P10Speed      float64 `json:"p10_speed"`
	MinSpeed      float64 `json:"min_speed"`
	AvgRTT        float64 `json:"avg_rtt"`
	AvgLoss       float64 `json:"avg_loss"`
	AvgJitter     float64 `json:"avg_jitter"`
	AvgStability  float64 `json:"avg_stability"`
	StallRate     float64 `json:"stall_rate"`
	FailRate      float64 `json:"fail_rate"`
	PeakHourScore float64 `json:"peak_hour_score"`
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

	// Keep samples up to 24 hours (max 1000 samples to prevent memory unbounded growth)
	cutoff := s.Timestamp.Add(-24 * time.Hour)
	startIdx := 0
	for startIdx < len(r.Samples) && r.Samples[startIdx].Timestamp.Before(cutoff) {
		startIdx++
	}
	if startIdx > 0 {
		r.Samples = r.Samples[startIdx:]
	}
	if len(r.Samples) > 1000 {
		r.Samples = r.Samples[len(r.Samples)-1000:]
	}

	// 2. Update EWMA
	if s.Success {
		stallRate := 0.0
		if s.TotalStallDuration > 0 {
			stallRate = s.TotalStallDuration / 10.0
			if stallRate > 1.0 {
				stallRate = 1.0
			}
		}
		r.EWMA.Record(s.Speed, s.P10Speed, s.MinSpeed, s.RTT, s.PacketLoss, s.Jitter, s.Stability, stallRate, s.Timestamp)
	} else {
		r.EWMA.RecordFailure(s.Timestamp)
	}

	// 3. Update PeakHour stats (0..23)
	h := s.Timestamp.Hour()
	hs := &r.PeakHours[h]
	hs.Count++
	if !s.Success {
		hs.FailCount++
	} else {
		successCount := float64(hs.Count - hs.FailCount)
		if successCount <= 1 {
			hs.AvgSpeed = s.Speed
			hs.MedianSpeed = s.MedianSpeed
			hs.P10Speed = s.P10Speed
			hs.MinSpeed = s.MinSpeed
			hs.AvgRTT = s.RTT
			hs.AvgLoss = s.PacketLoss
			hs.AvgJitter = s.Jitter
			hs.AvgStability = s.Stability
			if s.TotalStallDuration > 0 {
				hs.StallRate = s.TotalStallDuration / 10.0
			}
		} else {
			hs.AvgSpeed += (s.Speed - hs.AvgSpeed) / successCount
			if s.MedianSpeed > 0 {
				hs.MedianSpeed += (s.MedianSpeed - hs.MedianSpeed) / successCount
			}
			if s.P10Speed > 0 {
				hs.P10Speed += (s.P10Speed - hs.P10Speed) / successCount
			}
			if s.MinSpeed < hs.MinSpeed {
				hs.MinSpeed = s.MinSpeed
			}
			hs.AvgRTT += (s.RTT - hs.AvgRTT) / successCount
			hs.AvgLoss += (s.PacketLoss - hs.AvgLoss) / successCount
			hs.AvgJitter += (s.Jitter - hs.AvgJitter) / successCount
			hs.AvgStability += (s.Stability - hs.AvgStability) / successCount
			sampleStallRate := s.TotalStallDuration / 10.0
			hs.StallRate += (sampleStallRate - hs.StallRate) / successCount
		}
	}
	if hs.Count > 0 {
		hs.FailRate = float64(hs.FailCount) / float64(hs.Count)
	}
	hs.PeakHourScore = CalcPeakHourScore(hs)
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

	// Confidence calculation: gradually builds from 20 up to 100
	if total < 3 {
		ws.Confidence = 20.0
	} else if total < 6 {
		ws.Confidence = 50.0
	} else if total < 12 {
		ws.Confidence = 80.0
	} else {
		ws.Confidence = math.Min(100.0, 80.0+float64(total-12)*2.0)
	}

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

	rec.AddSample(sample)

	// Update health status with baseline P10
	baselineP10 := rec.EWMA.LongSnapshot().P10Speed
	if baselineP10 <= 0 {
		baselineP10 = rec.EWMA.LongSnapshot().Speed
	}
	m.UpdateHealth(success, baselineP10)

	// Update EWMA snapshot
	snap := rec.EWMA.ShortSnapshot()
	m.EWMA = &snap

	// Evaluate confidence
	w15 := rec.CalcWindowStats(15*time.Minute, sample.Timestamp)
	m.Confidence = w15.Confidence
	if m.Confidence <= 0 {
		m.Confidence = 30.0
	}

	// Update current hour's PeakHourScore
	h := sample.Timestamp.Hour()
	m.PeakHourScore = rec.PeakHours[h].PeakHourScore

	// Evaluate multi-horizon scores
	penalty := rec.PeakHourPenalty(h)
	GlobalScoreEngine.EvaluateRoute(&m, snap, rec.EWMA.LongSnapshot(), penalty)

	// Assign grade and recommendation
	m.StabilityGrade = m.DetermineStabilityGrade()
	recType, reasons := m.GenerateRecommendation()
	m.Recommendation = recType
	m.RecommendationReasons = reasons

	// Commit updated metrics into record
	rec.Metrics = m
}

func (s *RouteStore) Get(idOrIP string) (*RouteRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.routes[idOrIP]
	return rec, ok
}

// getAllLocked returns all unique RouteMetrics without acquiring locks (call while holding mu).
func (s *RouteStore) getAllLocked() []RouteMetrics {
	seen := make(map[string]bool)
	var list []RouteMetrics
	for _, rec := range s.routes {
		if !seen[rec.Metrics.IP] {
			seen[rec.Metrics.IP] = true
			list = append(list, rec.Metrics)
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
	curHour := time.Now().Hour()
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
		// 1. Exclude FAILED and FAILING routes
		if m.Health == HealthFailed || m.Health == HealthFailing {
			continue
		}

		effective := m.FinalScore

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

// SnapshotContainer stores full state for robust serialization.
type SnapshotContainer struct {
	Version   string                  `json:"version"`
	Timestamp time.Time               `json:"timestamp"`
	Routes    map[string]*RouteRecord `json:"routes"`
}

// SaveSnapshot serializes full state to a JSON file atomically (Windows safe).
func (s *RouteStore) SaveSnapshot(path string) error {
	s.mu.RLock()
	uniqueRecords := make(map[string]*RouteRecord)
	for ip, rec := range s.routes {
		if rec != nil && rec.Metrics.IP == ip {
			uniqueRecords[ip] = rec
		}
	}
	s.mu.RUnlock()

	container := SnapshotContainer{
		Version:   "2.1.0",
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
