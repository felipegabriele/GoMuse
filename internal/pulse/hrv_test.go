package pulse

import (
	"math"
	"testing"
)

// addIntervals feeds beats spaced by the given millisecond intervals, starting
// at from, and returns the timestamp of the final beat.
//
// It takes an explicit start rather than defaulting to a constant so that a
// second call continues where the first stopped. Restarting at a fixed t would
// walk time backwards, and HRV would reject the resulting negative interval —
// making a test pass or fail for reasons unrelated to what it claims to check.
func addIntervals(h *HRV, from float64, ms []float64) float64 {
	t := from
	h.AddBeat(t)
	for _, d := range ms {
		t += d / 1000
		h.AddBeat(t)
	}
	return t
}

// The golden RMSSD, computed by hand from the interval sequence below:
//
//	intervals 800 840 810 850 820 860
//	diffs       40 -30  40 -30  40
//	none excluded: the largest ratio is 40/800 = 5%, under EctopicRatio
//	squares   1600 900 1600 900 1600  -> sum 6600, mean 1320
//	RMSSD = sqrt(1320) = 36.331804...
func TestRMSSDOfGoldenSequence(t *testing.T) {
	got, ok := rmssdOf([]float64{800, 840, 810, 850, 820, 860})
	if !ok {
		t.Fatal("rmssdOf reported not enough data for 6 intervals")
	}
	want := math.Sqrt(1320)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("rmssdOf = %.9f, want %.9f", got, want)
	}
}

func TestRMSSDExcludesEctopicDifferences(t *testing.T) {
	// A doubled interval in the middle. Both differences it takes part in
	// exceed EctopicRatio and must be excluded, leaving the surrounding steady
	// beats to dominate.
	clean, _ := rmssdOf([]float64{800, 800, 800, 800, 800})
	withEctopic, ok := rmssdOf([]float64{800, 800, 1600, 800, 800})
	if !ok {
		t.Fatal("rmssdOf reported not enough data")
	}
	if clean != 0 {
		t.Fatalf("rmssdOf of a constant sequence = %v, want 0", clean)
	}
	if withEctopic != 0 {
		t.Errorf("rmssdOf with an ectopic beat = %v, want 0: both differences "+
			"across it should have been excluded", withEctopic)
	}
}

func TestMedianOfEvenAndOddCounts(t *testing.T) {
	if got := medianOf([]float64{810, 800, 820}); got != 810 {
		t.Errorf("medianOf odd = %v, want 810", got)
	}
	// Even count averages the two middle values: 800 810 820 840 850 860.
	if got := medianOf([]float64{800, 840, 810, 850, 820, 860}); got != 830 {
		t.Errorf("medianOf even = %v, want 830", got)
	}
}

func TestBPMFromGoldenSequence(t *testing.T) {
	h := NewHRV()
	addIntervals(h, 1000, []float64{800, 840, 810, 850, 820, 860})

	got, ok := h.BPM()
	if !ok {
		t.Fatal("BPM not reported after 6 intervals, want a value")
	}
	want := 60000.0 / 830.0 // median 830 ms
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("BPM = %.9f, want %.9f", got, want)
	}
}

func TestBPMWithheldUntilLocked(t *testing.T) {
	h := NewHRV()
	// Four intervals: one short of LockIntervals.
	end := addIntervals(h, 1000, []float64{800, 800, 800, 800})
	if _, ok := h.BPM(); ok {
		t.Errorf("BPM reported with %d intervals, want none before %d",
			h.IntervalCount(), LockIntervals)
	}
	h.AddBeat(end + 0.8)
	if _, ok := h.BPM(); !ok {
		t.Error("BPM not reported once LockIntervals intervals exist")
	}
}

func TestIntervalsOutsideBoundsRejected(t *testing.T) {
	h := NewHRV()
	h.AddBeat(1000)
	h.AddBeat(1000.1) // 100 ms, below MinIntervalMs
	h.AddBeat(1003.1) // 3000 ms, above MaxIntervalMs
	if got := h.IntervalCount(); got != 0 {
		t.Errorf("IntervalCount = %d, want 0: both intervals are out of bounds", got)
	}
}

// Break marks a data-loss boundary. No interval may be formed across it, even
// when the elapsed time would pass the bounds check.
func TestBreakPreventsIntervalAcrossGap(t *testing.T) {
	h := NewHRV()
	h.AddBeat(1000)
	h.AddBeat(1000.8)
	if got := h.IntervalCount(); got != 1 {
		t.Fatalf("IntervalCount = %d, want 1 before the break", got)
	}

	h.Break()
	h.AddBeat(1001.6) // 800 ms after the last beat: would otherwise be accepted
	if got := h.IntervalCount(); got != 1 {
		t.Errorf("IntervalCount = %d, want 1: no interval may span a break", got)
	}

	h.AddBeat(1002.4) // contiguous again
	if got := h.IntervalCount(); got != 2 {
		t.Errorf("IntervalCount = %d, want 2 once beats are contiguous again", got)
	}
}

// RMSSD must not difference the last interval before a Break against the first
// one after it. Those two intervals are adjacent in the ring but not successive
// in the signal, and a step between two internally-steady segments is exactly
// the sort of small difference the ectopic filter lets through — so without the
// chain tag it lands straight in the metric.
func TestRMSSDIgnoresPairsSpanningABreak(t *testing.T) {
	h := NewHRV()
	steady := func(ms float64, n int) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = ms
		}
		return out
	}

	end := addIntervals(h, 1000, steady(800, 20))
	h.Break()
	addIntervals(h, end+3.0, steady(820, 20)) // a 3 s hole, then a second clean run

	if got := h.IntervalCount(); got != 40 {
		t.Fatalf("IntervalCount = %d, want 40: both runs' intervals should be kept", got)
	}
	got, ok := h.RMSSD()
	if !ok {
		t.Fatal("RMSSD not reported over two runs spanning 35 s")
	}
	// Every difference within each run is 0. The only nonzero candidate is the
	// 800 -> 820 step across the break, whose 2.5% ratio passes the ectopic
	// filter, so anything above 0 means that pair was counted. The tolerance
	// absorbs the ~1e-11 ms of float error in accumulating beat times; counting
	// the spanning pair would give about 3.2 ms.
	if got > 1e-6 {
		t.Errorf("RMSSD = %v over two internally-steady runs, want 0: a pair "+
			"spanning the break was differenced", got)
	}
}

func TestRMSSDWithheldUntilSpanAccumulates(t *testing.T) {
	h := NewHRV()
	// Six intervals is about 5 s, well under RMSSDMinSpanSec.
	end := addIntervals(h, 1000, []float64{800, 840, 810, 850, 820, 860})
	if _, ok := h.RMSSD(); ok {
		t.Error("RMSSD reported after ~5 s, want none before 30 s of intervals")
	}

	// Continue from where the first run stopped, past 30 s total.
	long := make([]float64, 40)
	for i := range long {
		long[i] = 800
	}
	addIntervals(h, end+0.8, long)
	if _, ok := h.RMSSD(); !ok {
		t.Error("RMSSD not reported after 30+ s of intervals")
	}
}

// TestRMSSDGapDoesNotCountTowardWarmup guards the RMSSDMinSpanSec gate against
// a data outage substituting for beats. Two five-and-a-bit second runs either
// side of a 25 s hole are 36.2 s apart end to end, so a gate measuring elapsed
// time between the oldest and newest interval would open — and report RMSSD off
// 11.2 s of actual beats, the very case the gate exists to withhold.
//
// The scenario is reachable without any test-only contrivance: during a total
// outage Tracker.Process is never called, so nothing resets the accumulator,
// and the Detector's gap return calls Break() when data resumes. Both runs stay
// in the window because 36.2 s is under RMSSDWindowSec.
func TestRMSSDGapDoesNotCountTowardWarmup(t *testing.T) {
	h := NewHRV()
	steady := make([]float64, 7)
	for i := range steady {
		steady[i] = 800
	}

	end := addIntervals(h, 1000, steady) // 7 intervals, 5.6 s of coverage
	h.Break()
	addIntervals(h, end+25.0, steady) // 25 s hole, then 5.6 s more

	// Well past LockIntervals, so the count check is not what is withholding.
	if got := h.IntervalCount(); got != 14 {
		t.Fatalf("IntervalCount = %d, want 14: both runs should still be in the window", got)
	}
	if got := h.ivals[len(h.ivals)-1].at - h.ivals[0].at; got < RMSSDMinSpanSec {
		t.Fatalf("elapsed span = %.2fs, want >= %v: the test must actually exercise "+
			"the case where wall-clock passes but coverage does not", got, RMSSDMinSpanSec)
	}
	if got, ok := h.RMSSD(); ok {
		t.Errorf("RMSSD = %v reported off 11.2 s of beats spread over 36.2 s, want "+
			"none: the 25 s gap must not count toward RMSSDMinSpanSec", got)
	}

	// And it does open once the missing coverage is genuinely observed: another
	// ~19 s of clean beats takes the total past 30 s.
	more := make([]float64, 24)
	for i := range more {
		more[i] = 800
	}
	addIntervals(h, h.ivals[len(h.ivals)-1].at+0.8, more)
	if _, ok := h.RMSSD(); !ok {
		t.Error("RMSSD not reported once 30+ s of beats have actually been seen")
	}
}

func TestBeatsReturnsRecentOnly(t *testing.T) {
	h := NewHRV()
	for i := 0; i < 10; i++ {
		h.AddBeat(1000 + float64(i))
	}
	got := h.Beats(1005)
	if len(got) != 5 {
		t.Fatalf("Beats(1005) returned %d beats, want 5 (t=1005..1009)", len(got))
	}
	if got[0] != 1005 {
		t.Errorf("Beats(1005)[0] = %v, want 1005", got[0])
	}
}

// TestBeatHistoryEvictsStaleBeats pins the BeatHistorySec trim on the
// underlying beat storage, not just the since filter Beats() applies on top.
//
// 90 one-second intervals carry t from 1000 to 1090 (91 beats). The trim in
// AddBeat runs on every call against the *current* t, and the cutoff only
// climbs as t climbs, so by the end only beats with t >= 1090-60 = 1030
// survive: 61 of the 91 fed. Beats(0) is called with since far in the past —
// if eviction were broken (or Beats() were merely filtering a full history),
// it would still return all 91; the assertions below fail either way.
func TestBeatHistoryEvictsStaleBeats(t *testing.T) {
	h := NewHRV()
	ms := make([]float64, 90)
	for i := range ms {
		ms[i] = 1000
	}
	end := addIntervals(h, 1000, ms)

	got := h.Beats(0) // since far in the past: exercises storage, not the filter
	for _, bt := range got {
		if bt < end-BeatHistorySec {
			t.Fatalf("Beats(0) returned stale beat t=%v, want nothing older than %v",
				bt, end-BeatHistorySec)
		}
	}
	if len(got) != 61 {
		t.Errorf("Beats(0) returned %d beats, want 61 (t=1030..1090 survive a 60s history)", len(got))
	}
	if len(got) != len(h.beats) {
		t.Errorf("Beats(0) returned %d but h.beats holds %d: since=0 should not "+
			"filter anything further once eviction has already run", len(got), len(h.beats))
	}
}

// TestIntervalWindowEvictsStaleIntervals pins the RMSSDWindowSec trim on
// h.ivals: IntervalCount must not grow without bound as beats keep arriving.
//
// Same 90 one-second intervals as above; each AddBeat call trims h.ivals
// against the current t. The interval closing at t=1000+i survives only if
// 1000+i >= 1090-60 = 1030, i.e. i >= 30, leaving 61 of the 90 fed intervals.
func TestIntervalWindowEvictsStaleIntervals(t *testing.T) {
	h := NewHRV()
	ms := make([]float64, 90)
	for i := range ms {
		ms[i] = 1000
	}
	addIntervals(h, 1000, ms)

	if got := h.IntervalCount(); got != 61 {
		t.Errorf("IntervalCount = %d, want 61: intervals older than RMSSDWindowSec "+
			"(90 fed, 60s window) should have been evicted", got)
	}
}
