package pulse

import (
	"math"

	"gomuse/internal/dsp"
)

const (
	// RefractorySec is the shortest accepted gap between beats. A real optical
	// channel can carry a broad reflected wave 300-400 ms after systole, not
	// just the small ~100 ms dicrotic notch seen in idealised PPG. Accepting
	// that second maximum is what makes a resting rate of roughly 67 bpm look
	// like 143-160 bpm. 450 ms rejects both maxima while retaining rates up to
	// 133 bpm, which covers the stationary use for which a headband pulse
	// reading is meaningful.
	RefractorySec = 0.45

	// PeakFraction of the running peak estimate is the detection threshold.
	PeakFraction = 0.5

	// PeakTauSec is how fast the peak estimate decays toward the current
	// signal. Three seconds is a few beats: long enough not to be dragged down
	// between beats, short enough to follow changing perfusion.
	PeakTauSec = 3.0

	// SmoothTaps is the moving-average length, ~78 ms at 64 Hz. Enough to kill
	// sample-rate noise without blunting the systolic rise.
	SmoothTaps = 5

	// ChainBreakSec and SignalResetSec are the two thresholds for a hole in the
	// stream. They are deliberately separate, and deliberately absolute seconds
	// rather than sample periods: both are about physiology and filter time
	// constants, not about the stream's rate.
	//
	// Data loss is routine here, not exceptional. internal/hub drops newest on a
	// full buffer and a browser subscriber drops routinely by design (CLAUDE.md),
	// and on a Muse S Athena optics tag 0x36 carries one sample per packet
	// (internal/muse/athena.go), so a single dropped chunk is a single missing
	// sample — a ~16 ms hole at 64 Hz. Reacting to that as lost data is what
	// made the feature unable to lock at all at about one drop per second:
	// every interval chain segment was then shorter than one beat period, so no
	// interval ever formed.
	//
	// ChainBreakSec: a hole only corrupts an *interval* if it is long enough to
	// shift or hide a beat. Beat times come from real timestamps, not sample
	// counts, so a hole of H seconds shifts a detected beat by at most H. RMSSD
	// measures successive differences on the order of 20-60 ms, and one
	// perturbed term among the ~70 in a 60 s window moves the result
	// negligibly, so holes up to 50 ms are tolerated rather than breaking the
	// chain. This is the tradeoff the spec's gap-detection section reasons
	// about, resolved in favour of a metric that can actually lock.
	ChainBreakSec = 0.05

	// SignalResetSec is the much longer hole at which the DC blocker's history
	// and the running peak estimate are genuinely stale and worth throwing
	// away. Both have time constants of seconds — dcCornerHz is 0.5 Hz and
	// PeakTauSec is 3 s — so half a second is where restarting them costs less
	// than carrying them. Resetting on every single lost sample was the
	// aggravating factor that turned a 16 ms hole into a lost beat, because it
	// discarded ~3 s of peak adaptation each time.
	SignalResetSec = 0.5

	// dcCornerHz is the DC blocker corner. The pedestal is ~100000 counts and
	// the pulse ~4000, so removing DC is what makes a fixed threshold possible
	// at all.
	//
	// A dsp.FirwinBandpass is deliberately not used: at 64 Hz a 0.7 Hz lower
	// edge needs roughly 512 taps and ~4 s of group delay, unusable live. This
	// is the same limitation dcblock.go documents for EEG, and the same remedy.
	dcCornerHz = 0.5
)

// Detector finds beats in one pulse channel.
type Detector struct {
	rate   float64
	period float64

	dc *dsp.DCBlocker

	hist []float64 // moving-average window
	sum  float64
	idx  int
	full bool

	peak      float64
	peakDecay float64

	// A beat is the middle of three smoothed samples, so two must be kept.
	prev, prevPrev float64
	prevTS         float64
	have           int

	// haveBeat is distinct from primed: primed only means Process has run
	// before (so lastTS is meaningful for gap detection), while haveBeat means
	// a beat has actually been accepted (so lastBeat is meaningful for the
	// refractory check). A block shorter than one pulse period can run
	// Process without finding a beat, and gating the refractory check on
	// primed would then test a genuine first beat against a stale lastBeat of
	// 0 — harmless on Unix-epoch timestamps but wrong on a zero-based clock.
	lastBeat float64
	haveBeat bool
	lastTS   float64
	primed   bool
}

// NewDetector returns a detector for a stream of the given rate.
func NewDetector(rate float64) *Detector {
	if rate <= 0 {
		panic("pulse: rate must be positive")
	}
	d := &Detector{
		rate:      rate,
		period:    1 / rate,
		hist:      make([]float64, SmoothTaps),
		peakDecay: math.Exp(-1 / (rate * PeakTauSec)),
	}
	d.dc = dsp.NewDCBlocker(dcCornerHz, rate)
	return d
}

// Process feeds one channel's samples and their timestamps, returning the beat
// timestamps found within this call. gap reports a hole longer than
// ChainBreakSec between the previous call and this one, so the caller must not
// form an interval across the boundary. Shorter holes are tolerated silently;
// see the constant's comment for why.
//
// samples and timestamps must be the same length and are not modified.
func (d *Detector) Process(samples, timestamps []float64) (beats []float64, gap bool) {
	if len(samples) == 0 || len(samples) != len(timestamps) {
		return nil, false
	}

	if d.primed {
		// hole is the missing time, not the arrival delta: consecutive samples
		// are one period apart, so that period is not a hole.
		hole := timestamps[0] - d.lastTS - d.period
		if hole > ChainBreakSec {
			// Long enough to shift or hide a beat, so the caller must not form
			// an interval across it.
			gap = true
		}
		if hole > SignalResetSec {
			// Long enough that the filter and peak state describe a signal that
			// no longer continues; start them over rather than carrying a stale
			// history across the discontinuity.
			d.resetSignalState()
		}
	}

	filtered := d.dc.Process(samples)

	for i, v := range filtered {
		s := d.smooth(v)

		if s > d.peak {
			d.peak = s
		} else {
			d.peak *= d.peakDecay
		}

		if d.have >= 2 {
			threshold := PeakFraction * d.peak
			isPeak := d.prev > d.prevPrev && d.prev >= s && d.prev > threshold
			if isPeak && (!d.haveBeat || d.prevTS-d.lastBeat >= RefractorySec) {
				beats = append(beats, d.prevTS)
				d.lastBeat = d.prevTS
				d.haveBeat = true
			}
		}

		d.prevPrev, d.prev = d.prev, s
		d.prevTS = timestamps[i]
		if d.have < 2 {
			d.have++
		}
	}

	d.lastTS = timestamps[len(timestamps)-1]
	d.primed = true
	return beats, gap
}

// Reset clears all state, as when the selected channel changes.
func (d *Detector) Reset() {
	d.resetSignalState()
	d.lastBeat, d.haveBeat, d.lastTS, d.primed = 0, false, 0, false
}

// resetSignalState clears everything derived from sample values, leaving the
// beat history alone.
func (d *Detector) resetSignalState() {
	d.dc.Reset()
	for i := range d.hist {
		d.hist[i] = 0
	}
	d.sum, d.idx, d.full = 0, 0, false
	d.peak = 0
	d.prev, d.prevPrev, d.have = 0, 0, 0
}

// smooth is a moving average over SmoothTaps samples.
func (d *Detector) smooth(v float64) float64 {
	d.sum -= d.hist[d.idx]
	d.hist[d.idx] = v
	d.sum += v
	d.idx++
	if d.idx == len(d.hist) {
		d.idx, d.full = 0, true
	}
	n := len(d.hist)
	if !d.full {
		n = d.idx
	}
	if n == 0 {
		return 0
	}
	return d.sum / float64(n)
}
