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
	Timestamp  time.Time `json:"timestamp"`
	Speed      float64   `json:"speed"`
	MinSpeed   float64   `json:"min_speed"`
	RTT        float64   `json:"rtt"`
	PacketLoss float64   `json:"packet_loss"`
	Jitter     float64   `json:"jitter"`
	Stability  float64   `json:"stability"`
	Success    bool      `json:"success"`
}

// WindowStats aggregates metrics over a given sliding window (e.g. 5m, 15m, 1h, 6h, 24h).
type WindowStats struct {
	Duration   string  `json:"duration"`
	Count      int     `json:"count"`
	AvgSpeed   float64 `json:"avg_speed"`
	MinSpeed   float64 `json:"min_speed"`
	AvgRTT     float64 `json:"avg_rtt"`
	AvgLoss    float64 `json:"avg_loss"`
	AvgJitter  float64 `json:"avg_jitter"`
	AvgStable  float64 `json:"avg_stability"`
	FailRate   float64 `json:"fail_rate"`
}

// HourStats tracks metrics aggregated for a specific hour of the day (00..23).
type HourStats struct {
	Hour       int     `json:"hour"`
	Count      int     `json:"count"`
	FailCount  int     `json:"fail_count"`
	AvgSpeed   float64 `json:"avg_speed"`
	MinSpeed   float64 `json:"min_speed"`
	AvgRTT     float64 `json:"avg_rtt"`
	AvgLoss    float64 `json:"avg_loss"`
	AvgJitter  float64 `json:"avg_jitter"`
	AvgStable  float64 `json:"avg_stability"`
	FailRate   float64 `json:"fail_rate"`
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
		r.PeakHours[h] = HourStats{Hour: h}
	}
	return r
}

// AddSample inserts a new measurement sample, updates PeakHour stats and EWMA, and trims older samples.
func (r *RouteRecord) AddSample(s MeasurementSample) {
	// 1. Append sample
	r.Samples = append(r.Samples, s)

	// Keep samples up to 24 hours
	cutoff := s.Timestamp.Add(-24 * time.Hour)
	startIdx := 0
	for startIdx < len(r.Samples) && r.Samples[startIdx].Timestamp.Before(cutoff) {
		startIdx++
	}
	if startIdx > 0 {
		r.Samples = r.Samples[startIdx:]
	}

	// 2. Update EWMA
	if s.Success {
		r.EWMA.Record(s.Speed, s.RTT, s.PacketLoss, s.Jitter, s.Stability, s.Timestamp)
	}

	// 3. Update PeakHour stats (0..23)
	h := s.Timestamp.Hour()
	hs := &r.PeakHours[h]
	hs.Count++
	if !s.Success {
		hs.FailCount++
	} else {
		// Incremental running averages
		successCount := float64(hs.Count - hs.FailCount)
		if successCount <= 1 {
			hs.AvgSpeed = s.Speed
			hs.MinSpeed = s.MinSpeed
			hs.AvgRTT = s.RTT
			hs.AvgLoss = s.PacketLoss
			hs.AvgJitter = s.Jitter
			hs.AvgStable = s.Stability
		} else {
			hs.AvgSpeed += (s.Speed - hs.AvgSpeed) / successCount
			if s.MinSpeed > 0 && (hs.MinSpeed == 0 || s.MinSpeed < hs.MinSpeed) {
				hs.MinSpeed = s.MinSpeed
			}
			hs.AvgRTT += (s.RTT - hs.AvgRTT) / successCount
			hs.AvgLoss += (s.PacketLoss - hs.AvgLoss) / successCount
			hs.AvgJitter += (s.Jitter - hs.AvgJitter) / successCount
			hs.AvgStable += (s.Stability - hs.AvgStable) / successCount
		}
	}
	if hs.Count > 0 {
		hs.FailRate = float64(hs.FailCount) / float64(hs.Count)
	}
}

// CalcWindowStats aggregates samples within the specified duration before now.
func (r *RouteRecord) CalcWindowStats(d time.Duration, now time.Time) WindowStats {
	cutoff := now.Add(-d)
	var sumSpeed, sumRTT, sumLoss, sumJitter, sumStable float64
	var minSpd float64 = math.MaxFloat64
	var validCount, failCount int

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
		if s.MinSpeed > 0 && s.MinSpeed < minSpd {
			minSpd = s.MinSpeed
		}
		sumRTT += s.RTT
		sumLoss += s.PacketLoss
		sumJitter += s.Jitter
		sumStable += s.Stability
	}

	total := validCount + failCount
	ws := WindowStats{Duration: d.String(), Count: total}
	if total == 0 {
		return ws
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
		ws.AvgStable = math.Round((sumStable/vc)*10) / 10
	}
	return ws
}

// PeakHourPenalty returns penalty points for this route at the given hour based on historical failure/speed drops.
func (r *RouteRecord) PeakHourPenalty(h int) float64 {
	hs := r.PeakHours[h]
	if hs.Count < 3 {
		return 0.0 // not enough samples
	}
	penalty := 0.0
	// High failure rate (>20%) penalty
	if hs.FailRate > 0.20 {
		penalty += hs.FailRate * 30.0
	}
	// High packet loss (>10%) penalty
	if hs.AvgLoss > 0.10 {
		penalty += hs.AvgLoss * 40.0
	}
	// High jitter (>20ms) penalty
	if hs.AvgJitter > 20.0 {
		penalty += (hs.AvgJitter - 20.0) * 0.5
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
	rec.Metrics.RTT = m.RTT
	rec.Metrics.PacketLoss = m.PacketLoss
	rec.Metrics.Jitter = m.Jitter
	rec.Metrics.DownloadSpeed = m.DownloadSpeed
	rec.Metrics.SingleSpeed = m.SingleSpeed
	rec.Metrics.MinSpeed = m.MinSpeed
	rec.Metrics.Stability = m.Stability
	rec.Metrics.LoadLatency = m.LoadLatency
	rec.Metrics.HandshakeSuccess = m.HandshakeSuccess
	rec.Metrics.InstantScore = m.InstantScore
	rec.Metrics.ShortTermScore = m.ShortTermScore
	rec.Metrics.LongTermScore = m.LongTermScore
	rec.Metrics.FinalScore = m.FinalScore
	rec.Metrics.ConsecutiveFails = m.ConsecutiveFails
	rec.Metrics.ConsecutiveSuccess = m.ConsecutiveSuccess
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

	// Create sample
	sample := MeasurementSample{
		Timestamp:  m.LastTested,
		Speed:      m.SingleSpeed,
		MinSpeed:   m.MinSpeed,
		RTT:        m.RTT,
		PacketLoss: m.PacketLoss,
		Jitter:     m.Jitter,
		Stability:  m.Stability,
		Success:    success,
	}
	if sample.Speed <= 0 {
		sample.Speed = m.DownloadSpeed
	}
	if sample.Timestamp.IsZero() {
		sample.Timestamp = time.Now()
	}

	rec.AddSample(sample)

	// Update health status
	baselineSpeed := rec.EWMA.LongSnapshot().Speed
	m.UpdateHealth(success, baselineSpeed)

	// Update EWMA snapshot in metrics
	snap := rec.EWMA.ShortSnapshot()
	m.EWMA = &snap

	// Evaluate multi-horizon scores
	penalty := rec.PeakHourPenalty(sample.Timestamp.Hour())
	GlobalScoreEngine.EvaluateRoute(&m, snap, rec.EWMA.LongSnapshot(), penalty)

	// Commit updated metrics into record
	rec.Metrics = m
}

func (s *RouteStore) Get(idOrIP string) (*RouteRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.routes[idOrIP]
	return rec, ok
}

func (s *RouteStore) GetAll() []RouteMetrics {
	s.mu.RLock()
	defer s.mu.RUnlock()

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

func (s *RouteStore) GetBest(limit int, colo string) []RouteMetrics {
	all := s.GetAll()
	var candidates []RouteMetrics
	for _, m := range all {
		if colo != "" && m.Colo != colo {
			continue
		}
		if m.Health == HealthFailed {
			continue
		}
		candidates = append(candidates, m)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].FinalScore > candidates[j].FinalScore
	})

	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
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

// SaveSnapshot serializes the store to a JSON file.
func (s *RouteStore) SaveSnapshot(path string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := json.MarshalIndent(s.GetAll(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// LoadSnapshot restores routes from a JSON snapshot file.
func (s *RouteStore) LoadSnapshot(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var list []RouteMetrics
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, m := range list {
		s.UpsertRoute(m)
	}
	return nil
}
