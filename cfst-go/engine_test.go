package main

import (
	"testing"
)

func TestProcessIntervalSamplesWithZeroSpeed(t *testing.T) {
	// 4 intervals of 1s each: 10, 8, 0 (stall), 9 MB/s
	intervals := []SpeedIntervalSample{
		{DeltaBytes: 10 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 10.0, IsStall: false},
		{DeltaBytes: 8 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 8.0, IsStall: false},
		{DeltaBytes: 0, DeltaDuration: 1.0, IntervalSpeed: 0.0, IsStall: true},
		{DeltaBytes: 9 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 9.0, IsStall: false},
	}
	totalBytes := int64(27 * 1024 * 1024)
	totalDuration := 4.0

	sm := ProcessIntervalSamples(intervals, totalBytes, totalDuration, 40.0, 2.0, 0.0)

	// Critical check: 0 MB/s interval MUST NOT be filtered out
	if sm.MinSpeed != 0.0 {
		t.Fatalf("MinSpeed MUST be 0.0 when a stall occurred, got %.2f", sm.MinSpeed)
	}
	if sm.ZeroSpeedIntervals != 1 {
		t.Fatalf("expected 1 ZeroSpeedInterval, got %d", sm.ZeroSpeedIntervals)
	}
	if sm.StallCount != 1 {
		t.Fatalf("expected StallCount=1, got %d", sm.StallCount)
	}
	if sm.TotalStallDuration != 1.0 {
		t.Fatalf("expected TotalStallDuration=1.0s, got %.2f", sm.TotalStallDuration)
	}
	if sm.StallRate != 0.25 {
		t.Fatalf("expected StallRate=0.25 (1s/4s), got %.2f", sm.StallRate)
	}
	if sm.P10Speed >= 8.0 {
		t.Fatalf("P10Speed must be pulled down by 0.0 stall interval, got %.2f", sm.P10Speed)
	}
}

func TestProcessIntervalSamplesExtremeDipAndStability(t *testing.T) {
	// Node 1: Dips severely (50, 50, 50, 0, 50)
	intervalsFluctuating := []SpeedIntervalSample{
		{DeltaBytes: 50 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 50.0, IsStall: false},
		{DeltaBytes: 50 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 50.0, IsStall: false},
		{DeltaBytes: 50 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 50.0, IsStall: false},
		{DeltaBytes: 0, DeltaDuration: 1.0, IntervalSpeed: 0.0, IsStall: true},
		{DeltaBytes: 50 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 50.0, IsStall: false},
	}
	smFluc := ProcessIntervalSamples(intervalsFluctuating, 200*1024*1024, 5.0, 50.0, 15.0, 0.02)

	// Node 2: Rock-solid consistent (55, 54, 56, 55, 55)
	intervalsSolid := []SpeedIntervalSample{
		{DeltaBytes: 55 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 55.0, IsStall: false},
		{DeltaBytes: 54 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 54.0, IsStall: false},
		{DeltaBytes: 56 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 56.0, IsStall: false},
		{DeltaBytes: 55 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 55.0, IsStall: false},
		{DeltaBytes: 55 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 55.0, IsStall: false},
	}
	smSolid := ProcessIntervalSamples(intervalsSolid, 275*1024*1024, 5.0, 40.0, 1.5, 0.0)

	if smSolid.Stability <= smFluc.Stability {
		t.Fatalf("Rock solid route stability (%.1f) must exceed fluctuating route stability (%.1f)",
			smSolid.Stability, smFluc.Stability)
	}

	if smSolid.CoefficientOfVariation >= smFluc.CoefficientOfVariation {
		t.Fatalf("CV of solid route (%.3f) must be lower than fluctuating route (%.3f)",
			smSolid.CoefficientOfVariation, smFluc.CoefficientOfVariation)
	}

	if smFluc.MinSpeed != 0.0 {
		t.Fatalf("Fluctuating route min speed must be 0.0, got %.2f", smFluc.MinSpeed)
	}
	if smSolid.MinSpeed != 54.0 {
		t.Fatalf("Solid route min speed must be 54.0, got %.2f", smSolid.MinSpeed)
	}
}

func TestLongestStallDurationVariableIntervals(t *testing.T) {
	// interval 1 = 1s stall
	// interval 2 = 2s stall
	// interval 3 = 1.5s stall
	// total consecutive stall = 4.5s
	intervals := []SpeedIntervalSample{
		{DeltaBytes: 10 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 10.0, IsStall: false},
		{DeltaBytes: 0, DeltaDuration: 1.0, IntervalSpeed: 0.0, IsStall: true},
		{DeltaBytes: 0, DeltaDuration: 2.0, IntervalSpeed: 0.0, IsStall: true},
		{DeltaBytes: 0, DeltaDuration: 1.5, IntervalSpeed: 0.0, IsStall: true},
		{DeltaBytes: 15 * 1024 * 1024, DeltaDuration: 1.0, IntervalSpeed: 15.0, IsStall: false},
	}
	sm := ProcessIntervalSamples(intervals, int64(25*1024*1024), 6.5, 30.0, 1.0, 0.0)

	if sm.LongestStallDuration != 4.5 {
		t.Fatalf("expected LongestStallDuration=4.5s, got %.2f", sm.LongestStallDuration)
	}
	if sm.TotalStallDuration != 4.5 {
		t.Fatalf("expected TotalStallDuration=4.5s, got %.2f", sm.TotalStallDuration)
	}
	if sm.StallCount != 1 {
		t.Fatalf("expected StallCount=1, got %d", sm.StallCount)
	}
}
