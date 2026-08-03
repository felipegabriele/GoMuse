package pulse

import (
	"math"
	"sort"
)

const (
	// MinIntervalMs and MaxIntervalMs bound a plausible beat-to-beat interval:
	// 200 bpm down to 30 bpm. Anything outside is a detection error, not a
	// heartbeat.
	MinIntervalMs = 300.0
	MaxIntervalMs = 2000.0

	// LockIntervals is how many clean intervals must accumulate before any
	// number is shown. Reporting a rate off one or two beats would be
	// confidently wrong.
	LockIntervals = 5

	// BPMWindow is how many recent intervals the displayed rate is taken over.
	BPMWindow = 7

	// RMSSDWindowSec is the analysis window, and RMSSDMinSpanSec how much must
	// accumulate before RMSSD is shown at all. RMSSD over a handful of seconds
	// is dominated by whichever beats happen to be in it.
	//
	// RMSSDMinSpanSec is measured as accumulated *beat coverage* — the total
	// duration actually spanned by accepted intervals — not as elapsed time
	// between the oldest and newest interval in the window. Those differ
	// whenever data was lost: a 25 s outage between two five-second runs has an
	// elapsed span of 35 s but only 10 s of observed beats, and reporting RMSSD
	// off that would be exactly the handful-of-seconds figure this gate exists
	// to suppress. Outages that short are routine (see the Detector's gap
	// return), and the RMSSDWindowSec trim keeps both segments, so the elapsed
	// reading passed the gate on any hole under about a minute.
	RMSSDWindowSec  = 60.0
	RMSSDMinSpanSec = 30.0

	// EctopicRatio is the fractional difference between successive intervals
	// above which the pair is treated as an artifact — a missed or extra beat —
	// and excluded from RMSSD. This biases RMSSD slightly low in exchange for
	// not letting one bad beat dominate a metric built from small differences.
	EctopicRatio = 0.20

	// BeatHistorySec is how long beat timestamps are kept for display. It
	// matches MAX_WINDOW in app.js, the viewer's longest selectable window.
	BeatHistorySec = 60.0
)

// interval is one accepted beat-to-beat gap, tagged with when it ended so the
// analysis window can be trimmed by time rather than by count, and with which
// unbroken run of beats it belongs to.
type interval struct {
	at float64 // Unix seconds of the closing beat
	ms float64

	// chain identifies the run of contiguous beats this interval came from.
	// Break starts a new one. RMSSD is a measure of *successive* differences, so
	// differencing the last interval of one run against the first of the next
	// would invent a variation that was never observed: two clean segments of
	// 800 ms and 820 ms either side of a hole would report an RMSSD of 2.857 ms
	// where the truth is 0.
	chain int
}

// HRV turns a stream of beat timestamps into a rate and an RMSSD figure.
type HRV struct {
	last     float64
	haveLast bool
	chain    int

	ivals []interval
	beats []float64
}

// NewHRV returns an empty HRV accumulator.
func NewHRV() *HRV { return &HRV{} }

// AddBeat records a beat at time t, in Unix seconds.
func (h *HRV) AddBeat(t float64) {
	h.beats = append(h.beats, t)
	if cut := t - BeatHistorySec; len(h.beats) > 0 {
		i := 0
		for i < len(h.beats) && h.beats[i] < cut {
			i++
		}
		h.beats = append(h.beats[:0], h.beats[i:]...)
	}

	if h.haveLast {
		if ms := (t - h.last) * 1000; ms >= MinIntervalMs && ms <= MaxIntervalMs {
			h.ivals = append(h.ivals, interval{at: t, ms: ms, chain: h.chain})
			cut := t - RMSSDWindowSec
			i := 0
			for i < len(h.ivals) && h.ivals[i].at < cut {
				i++
			}
			h.ivals = append(h.ivals[:0], h.ivals[i:]...)
		}
	}
	h.last, h.haveLast = t, true
}

// Break marks a discontinuity: the next beat starts a new interval chain rather
// than closing one across lost data. See the Detector's gap return.
func (h *HRV) Break() {
	h.haveLast = false
	h.chain++
}

// IntervalCount returns how many accepted intervals are in the window.
func (h *HRV) IntervalCount() int { return len(h.ivals) }

// BPM returns the current rate, and whether enough intervals exist to report
// one. The median of the recent intervals is used rather than the mean so that
// a single missed or doubled beat does not move the displayed number.
func (h *HRV) BPM() (float64, bool) {
	if len(h.ivals) < LockIntervals {
		return 0, false
	}
	n := BPMWindow
	if n > len(h.ivals) {
		n = len(h.ivals)
	}
	recent := make([]float64, n)
	for i, v := range h.ivals[len(h.ivals)-n:] {
		recent[i] = v.ms
	}
	m := medianOf(recent)
	if m <= 0 {
		return 0, false
	}
	return 60000 / m, true
}

// LastIntervalMs returns the most recent accepted interval.
func (h *HRV) LastIntervalMs() (float64, bool) {
	if len(h.ivals) == 0 {
		return 0, false
	}
	return h.ivals[len(h.ivals)-1].ms, true
}

// RMSSD returns the root mean square of successive differences over the window,
// and whether enough beat coverage has accumulated to report it.
func (h *HRV) RMSSD() (float64, bool) {
	if len(h.ivals) < LockIntervals {
		return 0, false
	}
	// Summing the intervals, rather than subtracting the first interval's time
	// from the last's, is what makes a data gap not count toward the warm-up:
	// the hole contributes no interval, so it contributes no coverage. See
	// RMSSDMinSpanSec.
	//
	// This deliberately does not consult interval.chain even though
	// rmssdOfIntervals right below genuinely must. Every interval is wholly
	// inside one chain by construction — AddBeat only appends while haveLast is
	// set, and Break clears haveLast as it bumps chain — so the plain sum is
	// already same-chain coverage. Chain boundaries matter when *differencing*
	// two intervals, which this is not doing.
	coverage := 0.0
	for _, v := range h.ivals {
		coverage += v.ms / 1000
	}
	if coverage < RMSSDMinSpanSec {
		return 0, false
	}
	return rmssdOfIntervals(h.ivals)
}

// Beats returns the beat timestamps at or after since, oldest first.
func (h *HRV) Beats(since float64) []float64 {
	out := make([]float64, 0, len(h.beats))
	for _, t := range h.beats {
		if t >= since {
			out = append(out, t)
		}
	}
	return out
}

// Reset clears all accumulated state.
func (h *HRV) Reset() {
	h.last, h.haveLast, h.chain = 0, false, 0
	h.ivals = h.ivals[:0]
	h.beats = h.beats[:0]
}

// medianOf returns the median of ms. It sorts a copy, so the caller's slice is
// untouched. For an even count it averages the two middle values.
func medianOf(ms []float64) float64 {
	if len(ms) == 0 {
		return 0
	}
	s := make([]float64, len(ms))
	copy(s, ms)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// rmssdOf is the root mean square of successive differences over a plain
// sequence of intervals, all treated as one unbroken chain. It exists as a pure
// function of a []float64 so the golden-value test can pin the arithmetic
// without constructing intervals.
func rmssdOf(ms []float64) (float64, bool) {
	ivals := make([]interval, len(ms))
	for i, v := range ms {
		ivals[i] = interval{ms: v}
	}
	return rmssdOfIntervals(ivals)
}

// rmssdOfIntervals is the same computation, but it also skips any pair spanning
// a chain boundary — see interval.chain. Pairs whose fractional difference
// exceeds EctopicRatio are excluded, and it reports false when too few
// differences survive to mean anything.
func rmssdOfIntervals(ivals []interval) (float64, bool) {
	if len(ivals) < 2 {
		return 0, false
	}
	sum, n := 0.0, 0
	for i := 1; i < len(ivals); i++ {
		if ivals[i].chain != ivals[i-1].chain {
			continue // successive in the slice, but not in the signal
		}
		a, b := ivals[i-1].ms, ivals[i].ms
		lo := math.Min(a, b)
		if lo <= 0 {
			continue
		}
		if math.Abs(a-b)/lo > EctopicRatio {
			continue // an artifact, not a beat-to-beat variation
		}
		d := a - b
		sum += d * d
		n++
	}
	if n < 2 {
		return 0, false
	}
	return math.Sqrt(sum / float64(n)), true
}
