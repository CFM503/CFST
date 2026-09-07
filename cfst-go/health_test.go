package main

import (
	"testing"
)

func TestRouteHealthStateMachine(t *testing.T) {
	m := &RouteMetrics{
		IP:               "1.2.3.4",
		Port:             443,
		RTT:              50.0,
		PacketLoss:       0.0,
		Jitter:           2.0,
		SingleSpeed:      50.0,
		HandshakeSuccess: true,
		Health:           HealthHealthy,
	}

	baselineSpeed := 50.0

	// 1. Degraded test: speed drops from 50 to 20 (>30% drop)
	m.SingleSpeed = 20.0
	m.UpdateHealth(true, baselineSpeed)
	if m.Health != HealthDegraded {
		t.Fatalf("expected DEGRADED on >30%% speed drop, got %s", m.Health)
	}

	// 2. Failure detection: 1 failure -> FAILING
	m.UpdateHealth(false, baselineSpeed)
	if m.Health != HealthFailing {
		t.Fatalf("expected FAILING on 1 failure, got %s", m.Health)
	}
	if m.ConsecutiveFails != 1 {
		t.Fatalf("expected 1 consecutive fail, got %d", m.ConsecutiveFails)
	}

	// 3. 2nd failure -> still FAILING
	m.UpdateHealth(false, baselineSpeed)
	if m.Health != HealthFailing {
		t.Fatalf("expected FAILING on 2nd failure, got %s", m.Health)
	}

	// 4. 3rd failure -> FAILED
	m.UpdateHealth(false, baselineSpeed)
	if m.Health != HealthFailed {
		t.Fatalf("expected FAILED on 3rd failure, got %s", m.Health)
	}
	if m.Tier != TierFailed {
		t.Fatalf("expected tier FAILED on 3rd failure, got %s", m.Tier)
	}

	// 5. Recovery: 1st success after failure -> RECOVERING
	m.SingleSpeed = 50.0
	m.PacketLoss = 0.0
	m.Jitter = 2.0
	m.UpdateHealth(true, baselineSpeed)
	if m.Health != HealthRecovering {
		t.Fatalf("expected RECOVERING on 1st success after failure, got %s", m.Health)
	}

	// 2nd success -> still RECOVERING
	m.UpdateHealth(true, baselineSpeed)
	if m.Health != HealthRecovering {
		t.Fatalf("expected RECOVERING on 2nd success, got %s", m.Health)
	}

	// 3rd success -> HEALTHY
	m.UpdateHealth(true, baselineSpeed)
	if m.Health != HealthHealthy {
		t.Fatalf("expected HEALTHY after 3 consecutive successful recovery probes, got %s", m.Health)
	}
}

func TestDegradationByLossAndJitter(t *testing.T) {
	m := &RouteMetrics{
		IP:               "1.2.3.4",
		Port:             443,
		RTT:              50.0,
		PacketLoss:       0.20, // 20% loss (>15% threshold)
		Jitter:           2.0,
		SingleSpeed:      50.0,
		HandshakeSuccess: true,
		Health:           HealthHealthy,
	}
	m.UpdateHealth(true, 50.0)
	if m.Health != HealthDegraded {
		t.Fatalf("expected DEGRADED on high packet loss, got %s", m.Health)
	}

	m.PacketLoss = 0.0
	m.Jitter = 30.0 // >25ms threshold
	m.UpdateHealth(true, 50.0)
	if m.Health != HealthDegraded {
		t.Fatalf("expected DEGRADED on high jitter, got %s", m.Health)
	}
}

func TestFromNodeResult(t *testing.T) {
	node := NodeResult{
		IP:            "104.16.1.1",
		Port:          443,
		Colo:          "LAX",
		TCPLatency:    65.0,
		DownloadSpeed: 40.0,
		SingleSpeed:   38.0,
		MinSpeed:      30.0,
		Stability:     92.0,
		Score:         88.5,
	}

	rm := FromNodeResult(node, TierStandby)
	if rm.IP != "104.16.1.1" || rm.Tier != TierStandby || rm.Colo != "LAX" {
		t.Fatalf("unexpected RouteMetrics from NodeResult: %+v", rm)
	}
	if !rm.HandshakeSuccess {
		t.Fatal("expected HandshakeSuccess=true for valid node")
	}
	if rm.LastTested.IsZero() {
		t.Fatal("expected non-zero LastTested")
	}

	back := rm.ToNodeResult()
	if back.IP != node.IP || back.Colo != node.Colo || back.TCPLatency != node.TCPLatency {
		t.Fatalf("ToNodeResult mismatch: %+v vs %+v", back, node)
	}
}
