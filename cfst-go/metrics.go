package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type RouteHealth string

const (
	HealthHealthy    RouteHealth = "HEALTHY"
	HealthDegraded   RouteHealth = "DEGRADED"
	HealthFailing    RouteHealth = "FAILING"
	HealthFailed     RouteHealth = "FAILED"
	HealthRecovering RouteHealth = "RECOVERING"
)

type RouteTier string

const (
	TierActive    RouteTier = "ACTIVE"
	TierStandby   RouteTier = "STANDBY"
	TierCandidate RouteTier = "CANDIDATE"
	TierFailed    RouteTier = "FAILED"
)

type StabilityGrade string

const (
	GradeStable      StabilityGrade = "STABLE"
	GradeFluctuating StabilityGrade = "FLUCTUATING"
	GradeDegraded    StabilityGrade = "DEGRADED"
	GradeUnstable    StabilityGrade = "UNSTABLE"
	GradeFailed      StabilityGrade = "FAILED"
)

type RouteRecommendation string

const (
	RecBest     RouteRecommendation = "BEST"
	RecGood     RouteRecommendation = "GOOD"
	RecUsable   RouteRecommendation = "USABLE"
	RecDegraded RouteRecommendation = "DEGRADED"
	RecAvoid    RouteRecommendation = "AVOID"
	RecFailed   RouteRecommendation = "FAILED"
)

// RouteMetrics is the unified data contract representing a route's measured quality.
type RouteMetrics struct {
	ID                    string              `json:"id"`
	IP                    string              `json:"ip"`
	Port                  int                 `json:"port"`
	Colo                  string              `json:"colo"`
	Tier                  RouteTier           `json:"tier"`
	Health                RouteHealth         `json:"health"`
	StabilityGrade        StabilityGrade      `json:"stability_grade"`
	Recommendation        RouteRecommendation `json:"recommendation"`
	RecommendationReasons []string            `json:"recommendation_reasons"`
	RTT                   float64             `json:"rtt"`            // ms (TCPLatency)
	PacketLoss            float64             `json:"packet_loss"`    // 0.0 - 1.0 (0% - 100%)
	Jitter                float64             `json:"jitter"`         // ms
	DownloadSpeed         float64             `json:"download_speed"` // MB/s
	SingleSpeed           float64             `json:"single_speed"`   // MB/s
	P10Speed              float64             `json:"p10_speed"`      // MB/s
	P25Speed              float64             `json:"p25_speed"`      // MB/s
	MedianSpeed           float64             `json:"median_speed"`   // MB/s
	MinSpeed              float64             `json:"min_speed"`      // MB/s (true minimum, includes 0)
	MaxSpeed              float64             `json:"max_speed"`      // MB/s
	StdDev                float64             `json:"std_dev"`
	CV                    float64             `json:"cv"`
	Stability             float64             `json:"stability"`    // 0.0 - 100.0 (Composite stability)
	LoadLatency           float64             `json:"load_latency"` // ms
	HandshakeSuccess      bool                `json:"handshake_success"`
	ZeroSpeedIntervals    int                 `json:"zero_speed_intervals"`
	StallCount            int                 `json:"stall_count"`
	TotalStallDuration    float64             `json:"total_stall_duration"`
	LongestStallDuration  float64             `json:"longest_stall_duration"`
	StallRate             float64             `json:"stall_rate"`
	DurationSeconds       float64             `json:"duration_seconds,omitempty"`
	BaselineP10           float64             `json:"baseline_p10"`
	SpeedDropPercent      float64             `json:"speed_drop_percent"`
	Confidence            float64             `json:"confidence"` // 0.0 - 100.0
	PeakHourScore         float64             `json:"peak_hour_score"`
	InstantScore          float64             `json:"instant_score"`
	ShortTermScore        float64             `json:"short_term_score"`
	LongTermScore         float64             `json:"long_term_score"`
	FinalScore            float64             `json:"final_score"`
	EWMA                  *EWMASnapshot       `json:"ewma,omitempty"`
	ConsecutiveFails      int                 `json:"consecutive_failures"`
	ConsecutiveSuccess    int                 `json:"consecutive_successes"`
	ConsecutiveDegraded   int                 `json:"consecutive_degraded"`
	IsStale               bool                `json:"is_stale"`
	ObservationDuration   float64             `json:"observation_duration_sec"` // Total seconds between first and last sample
	LastSuccess           time.Time           `json:"last_success,omitempty"`
	LastTested            time.Time           `json:"last_tested"`
	Timestamp             time.Time           `json:"timestamp"`
}

// GenerateRouteID creates a canonical route ID from IP and Port.
func GenerateRouteID(ip string, port int) string {
	sanitized := strings.ReplaceAll(ip, ".", "-")
	sanitized = strings.ReplaceAll(sanitized, ":", "-")
	return fmt.Sprintf("route-%s-%d", sanitized, port)
}

// UpdateHealth transitions the route health state machine based on recent probe results with de-bouncing.
func (m *RouteMetrics) UpdateHealth(success bool, baselineP10 float64) {
	const (
		failureThreshold  = 3
		recoveryThreshold = 3
		degradedThreshold = 3
	)

	m.BaselineP10 = baselineP10
	if !success || !m.HandshakeSuccess || m.RTT <= 0 {
		m.ConsecutiveFails++
		m.ConsecutiveSuccess = 0
		m.ConsecutiveDegraded = 0

		if m.ConsecutiveFails >= failureThreshold {
			m.Health = HealthFailed
			m.Tier = TierFailed
			m.StabilityGrade = GradeFailed
			m.Recommendation = RecFailed
		} else {
			m.Health = HealthFailing
		}
		return
	}

	// Probe succeeded
	m.ConsecutiveSuccess++
	m.ConsecutiveFails = 0

	// Check degradation triggers:
	// 1. Packet loss > 15%
	// 2. Jitter > 25ms
	// 3. P10 speed drop > 30% compared to baseline P10 (if baseline is established)
	// 4. Stalls observed in current probe
	isDegradedCondition := false
	if m.PacketLoss > 0.15 {
		isDegradedCondition = true
	}
	if m.Jitter > 25.0 {
		isDegradedCondition = true
	}
	if m.StallCount > 0 || m.ZeroSpeedIntervals > 0 {
		isDegradedCondition = true
	}

	effectiveP10 := m.P10Speed
	if effectiveP10 <= 0 && m.MinSpeed > 0 {
		effectiveP10 = m.MinSpeed
	}
	if effectiveP10 <= 0 && m.SingleSpeed > 0 {
		effectiveP10 = m.SingleSpeed
	}
	if baselineP10 > 2.0 && effectiveP10 >= 0 {
		drop := (1.0 - (effectiveP10 / baselineP10)) * 100.0
		if drop > 0 {
			m.SpeedDropPercent = math.Round(drop*10) / 10
		} else {
			m.SpeedDropPercent = 0
		}
		if m.SpeedDropPercent >= 30.0 {
			isDegradedCondition = true
		}
	} else {
		m.SpeedDropPercent = 0
	}

	if isDegradedCondition {
		m.ConsecutiveDegraded++
	} else {
		if m.ConsecutiveDegraded > 0 {
			m.ConsecutiveDegraded--
		}
	}

	switch m.Health {
	case HealthFailed, HealthFailing:
		m.Health = HealthRecovering
	case HealthRecovering:
		if m.ConsecutiveSuccess >= recoveryThreshold && m.ConsecutiveDegraded == 0 {
			m.Health = HealthHealthy
		}
	default:
		if m.ConsecutiveDegraded >= degradedThreshold {
			m.Health = HealthDegraded
		} else if m.ConsecutiveDegraded == 0 {
			m.Health = HealthHealthy
		}
	}
}

// DetermineStabilityGrade classifies route stability.
func (m *RouteMetrics) DetermineStabilityGrade() StabilityGrade {
	if m.Health == HealthFailed {
		return GradeFailed
	}
	if m.Health == HealthFailing {
		return GradeUnstable
	}
	if m.StallCount > 0 || m.PacketLoss >= 0.10 || m.Jitter >= 30.0 {
		return GradeUnstable
	}
	if m.Health == HealthDegraded || m.SpeedDropPercent >= 30.0 {
		return GradeDegraded
	}
	if m.CV >= 0.35 || (m.MedianSpeed > 0 && m.P10Speed < m.MedianSpeed*0.50) {
		return GradeFluctuating
	}
	return GradeStable
}

// GenerateRecommendation assigns a recommendation tier and explanatory reasons.
func (m *RouteMetrics) GenerateRecommendation() (RouteRecommendation, []string) {
	var reasons []string

	if m.Health == HealthFailed {
		reasons = append(reasons, fmt.Sprintf("FAILED: Route failed %d consecutive probes", m.ConsecutiveFails))
		return RecFailed, reasons
	}

	if m.Health == HealthFailing {
		reasons = append(reasons, fmt.Sprintf("FAILING: Route recently failed probe (fails: %d)", m.ConsecutiveFails))
		return RecAvoid, reasons
	}

	if m.StallRate > 0.10 || m.LongestStallDuration >= 2.0 {
		reasons = append(reasons, fmt.Sprintf("AVOID: Severe stall detected (stall rate: %.1f%%, longest: %.1fs)", m.StallRate*100, m.LongestStallDuration))
		return RecAvoid, reasons
	}

	if m.PacketLoss >= 0.15 {
		reasons = append(reasons, fmt.Sprintf("AVOID: High packet loss (%.1f%%)", m.PacketLoss*100))
		return RecAvoid, reasons
	}

	if m.Health == HealthDegraded {
		if m.SpeedDropPercent >= 30.0 {
			reasons = append(reasons, fmt.Sprintf("DEGRADED: Speed dropped by %.1f%% against baseline", m.SpeedDropPercent))
		}
		if m.Jitter > 25.0 {
			reasons = append(reasons, fmt.Sprintf("DEGRADED: High jitter (%.1fms)", m.Jitter))
		}
		return RecDegraded, reasons
	}

	if m.Health == HealthRecovering {
		reasons = append(reasons, fmt.Sprintf("USABLE: Route is recovering (%d/%d probe successes)", m.ConsecutiveSuccess, 3))
		return RecUsable, reasons
	}

	// For Healthy routes, evaluate score and metrics
	effectiveSpeed := m.SingleSpeed
	if effectiveSpeed <= 0 {
		effectiveSpeed = m.DownloadSpeed
	}

	if m.FinalScore >= 85.0 && m.Stability >= 80.0 && m.PacketLoss == 0 && m.Jitter <= 15.0 && m.StallCount == 0 {
		reasons = append(reasons,
			fmt.Sprintf("BEST: P10 speed %.1f MB/s, 0%% loss, jitter %.1fms, peak score %.1f, confidence %.0f%%",
				m.P10Speed, m.Jitter, m.PeakHourScore, m.Confidence),
		)
		return RecBest, reasons
	}

	if m.FinalScore >= 70.0 && m.Stability >= 65.0 && m.PacketLoss <= 0.05 && m.StallCount == 0 {
		reasons = append(reasons,
			fmt.Sprintf("GOOD: P10 speed %.1f MB/s, stability %.1f%%, loss %.1f%%",
				m.P10Speed, m.Stability, m.PacketLoss*100),
		)
		return RecGood, reasons
	}

	reasons = append(reasons,
		fmt.Sprintf("USABLE: Score %.1f, speed %.1f MB/s, stability %.1f%%",
			m.FinalScore, effectiveSpeed, m.Stability),
	)
	return RecUsable, reasons
}

// ToNodeResult converts RouteMetrics to legacy NodeResult for backward compatibility with CLI/CSV.
func (m *RouteMetrics) ToNodeResult() NodeResult {
	return NodeResult{
		IP:                 m.IP,
		Port:               m.Port,
		TCPLatency:         m.RTT,
		DownloadSpeed:      m.DownloadSpeed,
		SingleSpeed:        m.SingleSpeed,
		LoadLatency:        m.LoadLatency,
		Colo:               m.Colo,
		Score:              m.FinalScore,
		Jitter:             m.Jitter,
		Stability:          m.Stability,
		MinSpeed:           m.MinSpeed,
		P10Speed:           m.P10Speed,
		MedianSpeed:        m.MedianSpeed,
		StallCount:         m.StallCount,
		ZeroSpeedIntervals: m.ZeroSpeedIntervals,
		PacketLoss:         m.PacketLoss,
	}
}

// FromNodeResult populates a RouteMetrics struct from a NodeResult.
func FromNodeResult(n NodeResult, tier RouteTier) *RouteMetrics {
	now := time.Now()
	p10 := n.P10Speed
	if p10 <= 0 && n.SingleSpeed > 0 {
		p10 = n.MinSpeed
	}
	median := n.MedianSpeed
	if median <= 0 && n.SingleSpeed > 0 {
		median = n.SingleSpeed
	}

	rm := &RouteMetrics{
		ID:                 GenerateRouteID(n.IP, n.Port),
		IP:                 n.IP,
		Port:               n.Port,
		Colo:               n.Colo,
		Tier:               tier,
		Health:             HealthHealthy,
		StabilityGrade:     GradeStable,
		Recommendation:     RecGood,
		RTT:                n.TCPLatency,
		PacketLoss:         n.PacketLoss,
		Jitter:             n.Jitter,
		DownloadSpeed:      n.DownloadSpeed,
		SingleSpeed:        n.SingleSpeed,
		P10Speed:           p10,
		MedianSpeed:        median,
		MinSpeed:           n.MinSpeed,
		Stability:          n.Stability,
		LoadLatency:        n.LoadLatency,
		HandshakeSuccess:   n.Colo != "ERR" && n.Colo != "429" && n.TCPLatency > 0,
		StallCount:         n.StallCount,
		ZeroSpeedIntervals: n.ZeroSpeedIntervals,
		Confidence:         50.0,
		InstantScore:       n.Score,
		ShortTermScore:     n.Score,
		LongTermScore:      n.Score,
		FinalScore:         n.Score,
		ConsecutiveSuccess: 1,
		LastTested:         now,
		Timestamp:          now,
	}
	rm.StabilityGrade = rm.DetermineStabilityGrade()
	rec, reasons := rm.GenerateRecommendation()
	rm.Recommendation = rec
	rm.RecommendationReasons = reasons
	return rm
}
