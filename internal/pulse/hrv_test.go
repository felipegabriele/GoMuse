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
