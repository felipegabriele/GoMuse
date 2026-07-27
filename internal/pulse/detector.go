package pulse

import (
	"math"

	"gomuse/internal/dsp"
)

const (
	// RefractorySec is the shortest accepted gap between beats: 300 ms caps
	// detection at 200 bpm and, more usefully, rejects the dicrotic notch —
	// the secondary bump every real PPG waveform carries about 100 ms after
	// systole, which would otherwise double the reported rate.
	RefractorySec = 0.30

	// PeakFraction of the running peak estimate is the detection threshold.
	PeakFraction = 0.5

	// PeakTauSec is how fast the peak estimate decays toward the current
	// signal. Three seconds is a few beats: long enough not to be dragged down
	// between beats, short enough to follow changing perfusion.
	PeakTauSec = 3.0

	// SmoothTaps is the moving-average length, ~78 ms at 64 Hz. Enough to kill
	// sample-rate noise without blunting the systolic rise.
	SmoothTaps = 5

	// GapPeriods is how far apart two consecutive samples may be before the
	// stream is treated as having lost data. 1.5 periods tolerates ordinary
	// jitter in the dejittered timestamps while catching a single lost packet.
	GapPeriods = 1.5

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
// timestamps found within this call. gap reports that samples were lost between
// the previous call and this one, so the caller must not form an interval
// across the boundary.
//
// samples and timestamps must be the same length and are not modified.
func (d *Detector) Process(samples, timestamps []float64) (beats []float64, gap bool) {
	if len(samples) == 0 || len(samples) != len(timestamps) {
		return nil, false
	}

	if d.primed && timestamps[0]-d.lastTS > GapPeriods*d.period {
		// Data was lost. The filter and peak state describe a signal that no
		// longer continues, so start them over rather than carrying a stale
		// history across the discontinuity.
		gap = true
		d.resetSignalState()
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
