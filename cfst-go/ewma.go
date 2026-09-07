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
	Speed     float64 `json:"speed"`
	Latency   float64 `json:"latency"`
	Loss      float64 `json:"loss"`
	Jitter    float64 `json:"jitter"`
	Stability float64 `json:"stability"`
}

// RouteEWMATracker tracks both short-term and long-term EWMA for a route.
type RouteEWMATracker struct {
	mu sync.RWMutex

	// Short-term (alpha ~ 0.3 for faster reaction, ~5-15 min horizon)
	ShortSpeed     EWMAValue `json:"short_speed"`
	ShortLatency   EWMAValue `json:"short_latency"`
	ShortLoss      EWMAValue `json:"short_loss"`
	ShortJitter    EWMAValue `json:"short_jitter"`
	ShortStability EWMAValue `json:"short_stability"`

	// Long-term (alpha ~ 0.05 for smoothed long-term baseline)
	LongSpeed     EWMAValue `json:"long_speed"`
	LongLatency   EWMAValue `json:"long_latency"`
	LongLoss      EWMAValue `json:"long_loss"`
	LongJitter    EWMAValue `json:"long_jitter"`
	LongStability EWMAValue `json:"long_stability"`
}

func NewRouteEWMATracker(shortAlpha, longAlpha float64) *RouteEWMATracker {
	if shortAlpha <= 0 || shortAlpha > 1.0 {
		shortAlpha = 0.30
	}
	if longAlpha <= 0 || longAlpha > 1.0 {
		longAlpha = 0.05
	}
	return &RouteEWMATracker{
		ShortSpeed:     NewEWMAValue(shortAlpha),
		ShortLatency:   NewEWMAValue(shortAlpha),
		ShortLoss:      NewEWMAValue(shortAlpha),
		ShortJitter:    NewEWMAValue(shortAlpha),
		ShortStability: NewEWMAValue(shortAlpha),

		LongSpeed:     NewEWMAValue(longAlpha),
		LongLatency:   NewEWMAValue(longAlpha),
		LongLoss:      NewEWMAValue(longAlpha),
		LongJitter:    NewEWMAValue(longAlpha),
		LongStability: NewEWMAValue(longAlpha),
	}
}

func (t *RouteEWMATracker) Record(speed, latency, loss, jitter, stability float64, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ShortSpeed.Update(speed, now)
	t.ShortLatency.Update(latency, now)
	t.ShortLoss.Update(loss, now)
	t.ShortJitter.Update(jitter, now)
	t.ShortStability.Update(stability, now)

	t.LongSpeed.Update(speed, now)
	t.LongLatency.Update(latency, now)
	t.LongLoss.Update(loss, now)
	t.LongJitter.Update(jitter, now)
	t.LongStability.Update(stability, now)
}

func (t *RouteEWMATracker) ShortSnapshot() EWMASnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return EWMASnapshot{
		Speed:     t.ShortSpeed.Value,
		Latency:   t.ShortLatency.Value,
		Loss:      t.ShortLoss.Value,
		Jitter:    t.ShortJitter.Value,
		Stability: t.ShortStability.Value,
	}
}

func (t *RouteEWMATracker) LongSnapshot() EWMASnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return EWMASnapshot{
		Speed:     t.LongSpeed.Value,
		Latency:   t.LongLatency.Value,
		Loss:      t.LongLoss.Value,
		Jitter:    t.LongJitter.Value,
		Stability: t.LongStability.Value,
	}
}
