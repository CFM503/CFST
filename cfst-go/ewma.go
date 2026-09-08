package main

import (
	"sync"
	"time"
)

// EWMAValue tracks a single metric using Exponential Weighted Moving Average.
type EWMAValue struct {
	Value       float64   `json:"value"`
	Alpha       float64   `json:"alpha"`
	Initialized bool      `json:"initialized"`
	LastUpdate  time.Time `json:"last_update"`
}

func NewEWMAValue(alpha float64) EWMAValue {
	if alpha <= 0 || alpha > 1.0 {
		alpha = 0.2
	}
	return EWMAValue{Alpha: alpha}
}

func (e *EWMAValue) Update(val float64, now time.Time) {
	if !e.Initialized {
		e.Value = val
		e.Initialized = true
		e.LastUpdate = now
		return
	}
	e.Value = e.Alpha*val + (1.0-e.Alpha)*e.Value
	e.LastUpdate = now
}

// EWMASnapshot is an immutable representation of current EWMA values.
type EWMASnapshot struct {
	Speed           float64 `json:"speed"`
	P10Speed        float64 `json:"p10_speed"`
	MinSpeed        float64 `json:"min_speed"`
	Latency         float64 `json:"latency"`
	Loss            float64 `json:"loss"`
	Jitter          float64 `json:"jitter"`
	Stability       float64 `json:"stability"`
	StallRate       float64 `json:"stall_rate"`
	FailurePressure float64 `json:"failure_pressure"`
}

// RouteEWMATracker tracks both short-term and long-term EWMA for a route.
type RouteEWMATracker struct {
	mu sync.RWMutex

	// Short-term (alpha ~ 0.30 for faster reaction, ~5-15 min horizon)
	ShortSpeed           EWMAValue `json:"short_speed"`
	ShortP10Speed        EWMAValue `json:"short_p10_speed"`
	ShortMinSpeed        EWMAValue `json:"short_min_speed"`
	ShortLatency         EWMAValue `json:"short_latency"`
	ShortLoss            EWMAValue `json:"short_loss"`
	ShortJitter          EWMAValue `json:"short_jitter"`
	ShortStability       EWMAValue `json:"short_stability"`
	ShortStallRate       EWMAValue `json:"short_stall_rate"`
	ShortFailurePressure EWMAValue `json:"short_failure_pressure"`

	// Long-term (alpha ~ 0.05 for smoothed long-term baseline, ~several hours)
	LongSpeed           EWMAValue `json:"long_speed"`
	LongP10Speed        EWMAValue `json:"long_p10_speed"`
	LongMinSpeed        EWMAValue `json:"long_min_speed"`
	LongLatency         EWMAValue `json:"long_latency"`
	LongLoss            EWMAValue `json:"long_loss"`
	LongJitter          EWMAValue `json:"long_jitter"`
	LongStability       EWMAValue `json:"long_stability"`
	LongStallRate       EWMAValue `json:"long_stall_rate"`
	LongFailurePressure EWMAValue `json:"long_failure_pressure"`
}

func NewRouteEWMATracker(shortAlpha, longAlpha float64) *RouteEWMATracker {
	if shortAlpha <= 0 || shortAlpha > 1.0 {
		shortAlpha = 0.30
	}
	if longAlpha <= 0 || longAlpha > 1.0 {
		longAlpha = 0.05
	}
	return &RouteEWMATracker{
		ShortSpeed:           NewEWMAValue(shortAlpha),
		ShortP10Speed:        NewEWMAValue(shortAlpha),
		ShortMinSpeed:        NewEWMAValue(shortAlpha),
		ShortLatency:         NewEWMAValue(shortAlpha),
		ShortLoss:            NewEWMAValue(shortAlpha),
		ShortJitter:          NewEWMAValue(shortAlpha),
		ShortStability:       NewEWMAValue(shortAlpha),
		ShortStallRate:       NewEWMAValue(shortAlpha),
		ShortFailurePressure: NewEWMAValue(shortAlpha),

		LongSpeed:           NewEWMAValue(longAlpha),
		LongP10Speed:        NewEWMAValue(longAlpha),
		LongMinSpeed:        NewEWMAValue(longAlpha),
		LongLatency:         NewEWMAValue(longAlpha),
		LongLoss:            NewEWMAValue(longAlpha),
		LongJitter:          NewEWMAValue(longAlpha),
		LongStability:       NewEWMAValue(longAlpha),
		LongStallRate:       NewEWMAValue(longAlpha),
		LongFailurePressure: NewEWMAValue(longAlpha),
	}
}

func (t *RouteEWMATracker) Record(speed, p10Speed, minSpeed, latency, loss, jitter, stability, stallRate float64, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ShortSpeed.Update(speed, now)
	t.ShortP10Speed.Update(p10Speed, now)
	t.ShortMinSpeed.Update(minSpeed, now)
	t.ShortLatency.Update(latency, now)
	t.ShortLoss.Update(loss, now)
	t.ShortJitter.Update(jitter, now)
	t.ShortStability.Update(stability, now)
	t.ShortStallRate.Update(stallRate, now)
	t.ShortFailurePressure.Update(0.0, now)

	t.LongSpeed.Update(speed, now)
	t.LongP10Speed.Update(p10Speed, now)
	t.LongMinSpeed.Update(minSpeed, now)
	t.LongLatency.Update(latency, now)
	t.LongLoss.Update(loss, now)
	t.LongJitter.Update(jitter, now)
	t.LongStability.Update(stability, now)
	t.LongStallRate.Update(stallRate, now)
	t.LongFailurePressure.Update(0.0, now)
}

// RecordFailure updates EWMA during failed probes, applying degradation pressure.
func (t *RouteEWMATracker) RecordFailure(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ShortFailurePressure.Update(100.0, now)
	t.LongFailurePressure.Update(100.0, now)

	t.ShortLoss.Update(1.0, now)
	t.LongLoss.Update(0.5, now)

	t.ShortStallRate.Update(1.0, now)
	t.LongStallRate.Update(0.3, now)

	t.ShortStability.Update(0.0, now)
	if t.LongStability.Initialized {
		t.LongStability.Value *= 0.80
		t.LongStability.LastUpdate = now
	}

	if t.ShortSpeed.Initialized {
		t.ShortSpeed.Value *= 0.65
		t.ShortSpeed.LastUpdate = now
	}
	if t.ShortP10Speed.Initialized {
		t.ShortP10Speed.Value *= 0.60
		t.ShortP10Speed.LastUpdate = now
	}
	if t.ShortMinSpeed.Initialized {
		t.ShortMinSpeed.Value = 0
		t.ShortMinSpeed.LastUpdate = now
	}

	if t.LongSpeed.Initialized {
		t.LongSpeed.Value *= 0.85
		t.LongSpeed.LastUpdate = now
	}
	if t.LongP10Speed.Initialized {
		t.LongP10Speed.Value *= 0.80
		t.LongP10Speed.LastUpdate = now
	}
	if t.LongMinSpeed.Initialized {
		t.LongMinSpeed.Value *= 0.50
		t.LongMinSpeed.LastUpdate = now
	}
}

func (t *RouteEWMATracker) ShortSnapshot() EWMASnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return EWMASnapshot{
		Speed:           t.ShortSpeed.Value,
		P10Speed:        t.ShortP10Speed.Value,
		MinSpeed:        t.ShortMinSpeed.Value,
		Latency:         t.ShortLatency.Value,
		Loss:            t.ShortLoss.Value,
		Jitter:          t.ShortJitter.Value,
		Stability:       t.ShortStability.Value,
		StallRate:       t.ShortStallRate.Value,
		FailurePressure: t.ShortFailurePressure.Value,
	}
}

func (t *RouteEWMATracker) LongSnapshot() EWMASnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return EWMASnapshot{
		Speed:           t.LongSpeed.Value,
		P10Speed:        t.LongP10Speed.Value,
		MinSpeed:        t.LongMinSpeed.Value,
		Latency:         t.LongLatency.Value,
		Loss:            t.LongLoss.Value,
		Jitter:          t.LongJitter.Value,
		Stability:       t.LongStability.Value,
		StallRate:       t.LongStallRate.Value,
		FailurePressure: t.LongFailurePressure.Value,
	}
}
