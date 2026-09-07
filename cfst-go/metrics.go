package main

import (
	"fmt"
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

// RouteMetrics is the unified data contract representing a route's measured quality.
type RouteMetrics struct {
	ID                 string        `json:"id"`
	IP                 string        `json:"ip"`
	Port               int           `json:"port"`
	Colo               string        `json:"colo"`
	Tier               RouteTier     `json:"tier"`
	Health             RouteHealth   `json:"health"`
	RTT                float64       `json:"rtt"` // ms (TCPLatency)
	PacketLoss         float64       `json:"packet_loss"` // 0.0 - 1.0 (0% - 100%)
	Jitter             float64       `json:"jitter"` // ms
	DownloadSpeed      float64       `json:"download_speed"` // MB/s
	SingleSpeed        float64       `json:"single_speed"` // MB/s
	MinSpeed           float64       `json:"min_speed"` // MB/s
	Stability          float64       `json:"stability"` // 0.0 - 100.0
	LoadLatency        float64       `json:"load_latency"` // ms
	HandshakeSuccess   bool          `json:"handshake_success"`
	InstantScore       float64       `json:"instant_score"`
	ShortTermScore     float64       `json:"short_term_score"`
	LongTermScore      float64       `json:"long_term_score"`
	FinalScore         float64       `json:"final_score"`
	EWMA               *EWMASnapshot `json:"ewma,omitempty"`
	ConsecutiveFails   int           `json:"consecutive_failures"`
	ConsecutiveSuccess int           `json:"consecutive_successes"`
	LastTested         time.Time     `json:"last_tested"`
	Timestamp          time.Time     `json:"timestamp"`
}

// GenerateRouteID creates a canonical route ID from IP and Port.
func GenerateRouteID(ip string, port int) string {
	sanitized := strings.ReplaceAll(ip, ".", "-")
	sanitized = strings.ReplaceAll(sanitized, ":", "-")
	return fmt.Sprintf("route-%s-%d", sanitized, port)
}

// UpdateHealth transitions the route health state machine based on recent probe results.
func (m *RouteMetrics) UpdateHealth(success bool, baselineSpeed float64) {
	if !success || !m.HandshakeSuccess || m.RTT <= 0 {
		m.ConsecutiveFails++
		m.ConsecutiveSuccess = 0
		if m.ConsecutiveFails >= 3 {
			m.Health = HealthFailed
			m.Tier = TierFailed
		} else {
			m.Health = HealthFailing
		}
		return
	}

	// Probe succeeded
	m.ConsecutiveSuccess++
	m.ConsecutiveFails = 0

	// Check for degradation:
	// 1. Packet loss > 15%
	// 2. Jitter > 25ms
	// 3. Speed drops by > 30% compared to baseline (if baseline is established)
	isDegraded := false
	if m.PacketLoss > 0.15 {
		isDegraded = true
	}
	if m.Jitter > 25.0 {
		isDegraded = true
	}
	if baselineSpeed > 5.0 && m.SingleSpeed > 0 && m.SingleSpeed < baselineSpeed*0.70 {
		isDegraded = true
	}

	switch m.Health {
	case HealthFailed, HealthFailing:
		m.Health = HealthRecovering
	case HealthRecovering:
		if m.ConsecutiveSuccess >= 3 && !isDegraded {
			m.Health = HealthHealthy
		}
	default:
		if isDegraded {
			m.Health = HealthDegraded
		} else {
			m.Health = HealthHealthy
		}
	}
}

// ToNodeResult converts RouteMetrics to legacy NodeResult for backward compatibility with CLI/CSV.
func (m *RouteMetrics) ToNodeResult() NodeResult {
	return NodeResult{
		IP:            m.IP,
		Port:          m.Port,
		TCPLatency:    m.RTT,
		DownloadSpeed: m.DownloadSpeed,
		SingleSpeed:   m.SingleSpeed,
		LoadLatency:   m.LoadLatency,
		Colo:          m.Colo,
		Score:         m.FinalScore,
		Jitter:        m.Jitter,
		Stability:     m.Stability,
		MinSpeed:      m.MinSpeed,
		PacketLoss:    m.PacketLoss,
	}
}

// FromNodeResult populates a RouteMetrics struct from a NodeResult.
func FromNodeResult(n NodeResult, tier RouteTier) *RouteMetrics {
	now := time.Now()
	rm := &RouteMetrics{
		ID:                 GenerateRouteID(n.IP, n.Port),
		IP:                 n.IP,
		Port:               n.Port,
		Colo:               n.Colo,
		Tier:               tier,
		Health:             HealthHealthy,
		RTT:                n.TCPLatency,
		PacketLoss:         n.PacketLoss,
		Jitter:             n.Jitter,
		DownloadSpeed:      n.DownloadSpeed,
		SingleSpeed:        n.SingleSpeed,
		MinSpeed:           n.MinSpeed,
		Stability:          n.Stability,
		LoadLatency:        n.LoadLatency,
		HandshakeSuccess:   n.Colo != "ERR" && n.Colo != "429" && n.TCPLatency > 0,
		InstantScore:       n.Score,
		ShortTermScore:     n.Score,
		LongTermScore:      n.Score,
		FinalScore:         n.Score,
		ConsecutiveSuccess: 1,
		LastTested:         now,
		Timestamp:          now,
	}
	return rm
}
