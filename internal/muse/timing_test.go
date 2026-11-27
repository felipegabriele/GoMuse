package muse

import (
	"math"
	"testing"
	"time"

	"gomuse/internal/sample"
)

func TestClockWatchIgnoresSlewing(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var got []ClockJump
	w := newClockWatch(start, func(j ClockJump) { got = append(got, j) })

	// An NTP client slewing gains a few milliseconds per second.
	for s := 1; s <= 60; s++ {
		mono := time.Duration(s) * time.Second
		w.check(start.Add(mono+time.Duration(s)*2*time.Millisecond), mono)
	}
	if len(got) != 0 || len(w.list()) != 0 {
		t.Fatalf("slewing was reported as a jump: %+v", got)
	}
}

func TestClockWatchReportsStepsBothWays(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var got []ClockJump
	w := newClockWatch(start, func(j ClockJump) { got = append(got, j) })

	w.check(start.Add(1*time.Second), 1*time.Second) // quiet
	// The wall clock jumps forward 2 s while only 1 s passed on the monotonic clock.
	w.check(start.Add(4*time.Second), 2*time.Second)
	// Then the wall clock is set back by 1 s while the monotonic clock moves on 1 s.
	w.check(start.Add(3*time.Second), 3*time.Second)

	if len(got) != 2 {
		t.Fatalf("got %d jumps, want 2: %+v", len(got), got)
	}
	if math.Abs(got[0].Ms-2000) > 1 {
		t.Errorf("first jump = %.1f ms, want +2000", got[0].Ms)
	}
	if math.Abs(got[1].Ms+2000) > 1 {
		t.Errorf("second jump = %.1f ms, want -2000", got[1].Ms)
	}
	if n := len(w.list()); n != 2 {
		t.Errorf("list has %d jumps, want 2", n)
	}
}

func TestDejitterTiming(t *testing.T) {
	d := NewDejitter(256, 12, 1000.5)
	d.Next(0, 1000.5)
	d.Next(1, 1000.5+12.0/256)

	tm := d.timing("eeg", 256)
	if tm.Method != "rls-fit" || tm.Clock != ClockHost || tm.Stream != "eeg" {
		t.Errorf("unexpected labels: %+v", tm)
	}
	if tm.AnchorUnixS != 1000.5 {
		t.Errorf("anchor = %v, want 1000.5", tm.AnchorUnixS)
	}
	if tm.Samples != 24 {
		t.Errorf("samples = %d, want 24", tm.Samples)
	}
	if math.Abs(tm.EstimatedRateHz*tm.EstimatedPeriodS-1) > 1e-9 || tm.EstimatedRateHz < 200 || tm.EstimatedRateHz > 300 {
		t.Errorf("implausible estimate: %+v", tm)
	}
}

func TestArrivalStat(t *testing.T) {
	var a arrivalStat
	if got := a.timing("acc", 52); got.Samples != 0 {
		t.Fatalf("empty stat has %d samples", got.Samples)
	}
	a.add(50.0, 3)
	a.add(51.0, 3)
	got := a.timing("acc", 52)
	if got.Method != "arrival" || got.AnchorUnixS != 50.0 || got.Samples != 6 || got.EstimatedRateHz != 0 {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestAssemblerTimingWaitsForFirstPacket(t *testing.T) {
	a := newAssembler(sample.PPG, func(sample.Chunk) {}, nil)
	if _, ok := a.timing(); ok {
		t.Fatal("timing reported before any packet completed")
	}
	n := a.nchan
	for ch := 0; ch < n; ch++ {
		a.add(7, ch, make([]float64, a.meta.SamplesPerPacket), 123.25)
	}
	tm, ok := a.timing()
	if !ok || tm.AnchorUnixS != 123.25 || tm.Stream != a.meta.Name {
		t.Errorf("unexpected after a packet: %+v ok=%v", tm, ok)
	}
}
