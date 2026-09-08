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

	// Record several samples (speed, p10Speed, minSpeed, latency, loss, jitter, stability, stallRate, timestamp)
	for i := 0; i < 5; i++ {
		tracker.Record(50.0, 45.0, 40.0, 40.0, 0.0, 2.0, 95.0, 0.0, now.Add(time.Duration(i)*time.Minute))
	}

	shortSnap := tracker.ShortSnapshot()
	longSnap := tracker.LongSnapshot()

	if shortSnap.Speed <= 0 || shortSnap.P10Speed <= 0 || shortSnap.MinSpeed <= 0 || shortSnap.Latency <= 0 || shortSnap.Stability <= 0 {
		t.Fatalf("short EWMA snapshot values missing: %+v", shortSnap)
	}
	if longSnap.Speed <= 0 || longSnap.P10Speed <= 0 || longSnap.MinSpeed <= 0 || longSnap.Latency <= 0 || longSnap.Stability <= 0 {
		t.Fatalf("long EWMA snapshot values missing: %+v", longSnap)
	}

	// Sudden speed drop in next measurement
	tracker.Record(10.0, 8.0, 5.0, 80.0, 0.1, 10.0, 50.0, 0.2, now.Add(6*time.Minute))

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

func TestRouteEWMAFailurePressure(t *testing.T) {
	now := time.Now()
	tracker := NewRouteEWMATracker(0.3, 0.05)

	// Route had good historical stats
	for i := 0; i < 5; i++ {
		tracker.Record(60.0, 55.0, 50.0, 30.0, 0.0, 1.0, 98.0, 0.0, now.Add(time.Duration(i)*time.Minute))
	}

	initSnap := tracker.ShortSnapshot()
	if initSnap.Speed < 50.0 {
		t.Fatalf("expected initial speed > 50, got %.1f", initSnap.Speed)
	}

	// Record failures
	failTime := now.Add(10 * time.Minute)
	tracker.RecordFailure(failTime)
	failSnap := tracker.ShortSnapshot()
	if failSnap.FailurePressure <= 0 {
		t.Fatalf("expected positive failure pressure after 1 fail, got %.2f", failSnap.FailurePressure)
	}

	// Speed should be decayed
	if failSnap.Speed >= initSnap.Speed {
		t.Fatalf("EWMA speed must decay upon failure: before=%.1f, after=%.1f", initSnap.Speed, failSnap.Speed)
	}
	if failSnap.Loss <= 0 {
		t.Fatalf("EWMA loss must increase upon failure, got %.2f", failSnap.Loss)
	}
}
