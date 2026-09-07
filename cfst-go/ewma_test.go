package main

import (
	"math"
	"testing"
	"time"
)

func TestEWMAValueUpdate(t *testing.T) {
	now := time.Now()
	// alpha = 0.5 for simple manual verification
	v := NewEWMAValue(0.5)

	if v.Initialized {
		t.Fatal("expected uninitialized initially")
	}

	v.Update(100.0, now)
	if !v.Initialized || v.Value != 100.0 {
		t.Fatalf("first update should set value directly, got %.1f", v.Value)
	}

	// Next update with 50.0 -> 0.5 * 50 + 0.5 * 100 = 75.0
	v.Update(50.0, now.Add(time.Second))
	if math.Abs(v.Value-75.0) > 0.001 {
		t.Fatalf("expected 75.0 after second update, got %.2f", v.Value)
	}

	// Next update with 50.0 -> 0.5 * 50 + 0.5 * 75 = 62.5
	v.Update(50.0, now.Add(2*time.Second))
	if math.Abs(v.Value-62.5) > 0.001 {
		t.Fatalf("expected 62.5 after third update, got %.2f", v.Value)
	}
}

func TestRouteEWMATracker(t *testing.T) {
	now := time.Now()
	tracker := NewRouteEWMATracker(0.3, 0.05)

	// Record several samples
	for i := 0; i < 5; i++ {
		tracker.Record(50.0, 40.0, 0.0, 2.0, 95.0, now.Add(time.Duration(i)*time.Minute))
	}

	shortSnap := tracker.ShortSnapshot()
	longSnap := tracker.LongSnapshot()

	if shortSnap.Speed <= 0 || shortSnap.Latency <= 0 || shortSnap.Stability <= 0 {
		t.Fatalf("short EWMA snapshot values missing: %+v", shortSnap)
	}
	if longSnap.Speed <= 0 || longSnap.Latency <= 0 || longSnap.Stability <= 0 {
		t.Fatalf("long EWMA snapshot values missing: %+v", longSnap)
	}

	// Sudden speed drop in next measurement
	tracker.Record(10.0, 80.0, 0.1, 10.0, 50.0, now.Add(6*time.Minute))

	newShort := tracker.ShortSnapshot()
	newLong := tracker.LongSnapshot()

	// Short-term should react faster to the drop than long-term
	if newShort.Speed >= shortSnap.Speed {
		t.Fatalf("short-term speed should have dropped, before=%.1f, after=%.1f", shortSnap.Speed, newShort.Speed)
	}
	if newLong.Speed > newShort.Speed {
		// Long-term has smaller alpha (0.05 vs 0.3), so long-term stays higher during sudden drop
	} else {
		t.Fatalf("long-term speed (%.1f) should be higher than short-term speed (%.1f) during sudden drop",
			newLong.Speed, newShort.Speed)
	}
}
