package dsp

import "math"

// DCBlocker is a one-pole/one-zero highpass that removes the constant offset
// and slow electrode drift from an EEG channel.
//
// It exists because the 32-tap FIR ported from muse-lsl does not actually
// reject DC: its "1 Hz" lower edge is roughly 26x narrower than a 32-tap
// Hamming design can resolve, so the kernel sums to ~0.87 rather than 0 and a
// 0.2 Hz wander passes at ~0.87 gain. See the response table in fir.go.
//
// Fixing that in the FIR would need ~512 taps and a full second of group
// delay, which is unusable for a live viewer. This costs one multiply-add per
// sample and effectively no delay.
//
// The recurrence is y[n] = x[n] - x[n-1] + r*y[n-1], whose pole sits just
// inside the unit circle at r. The -3 dB corner is at roughly (1-r)*fs/(2*pi).
//
// Measured amplitude response at fs=256, fc=0.5, alone and in series with the
// 32-tap FIR (see TestDCBlockerAttenuatesDrift and the fir.go table):
//
//	freq      FIR   blocker   chain
//	0.05 Hz  0.868   0.100    0.087
//	0.20 Hz  0.871   0.371    0.323
//	1.00 Hz  0.872   0.899    0.783
//	10.0 Hz  0.961   1.005    0.966
//	20.0 Hz  1.000   1.006    1.006
//	60.0 Hz  0.001   1.006    0.001
//
// So the chain drops slow wander by ~10x where the FIR alone did nothing,
// while leaving the 5-40 Hz band the FIR already handled essentially intact.
type DCBlocker struct {
	r     float64
	prevX float64
	prevY float64
	// primed tracks whether prevX has been seeded from real data. Starting
	// from a cold zero would emit a step equal to the first sample and take
	// seconds to decay — highly visible on a scrolling trace.
	primed bool
}

// NewDCBlocker returns a blocker with its -3 dB corner near fc Hz at a sample
// rate of fs Hz. It panics on a non-positive rate or a corner at or above
// Nyquist, both of which are programming errors rather than runtime conditions.
func NewDCBlocker(fc, fs float64) *DCBlocker {
	if fs <= 0 {
		panic("dsp: sample rate must be positive")
	}
	if fc <= 0 || fc >= fs/2 {
		panic("dsp: corner frequency must be in (0, fs/2)")
	}
	r := 1 - 2*math.Pi*fc/fs
	if r < 0 {
		r = 0
	}
	return &DCBlocker{r: r}
}

// Process filters x and returns a new slice, leaving the input untouched.
// State carries across calls, so consecutive chunks join seamlessly.
func (d *DCBlocker) Process(x []float64) []float64 {
	out := make([]float64, len(x))
	if len(x) == 0 {
		return out
	}
	if !d.primed {
		// Seed from the first sample so the filter starts settled: with
		// prevX == x[0] and prevY == 0 the first output is 0, which is the
		// steady-state response to a constant.
		d.prevX = x[0]
		d.prevY = 0
		d.primed = true
	}
	for i, v := range x {
		y := v - d.prevX + d.r*d.prevY
		d.prevX = v
		d.prevY = y
		out[i] = y
	}
	return out
}

// Reset returns the blocker to its unprimed state.
func (d *DCBlocker) Reset() {
	d.prevX, d.prevY, d.primed = 0, 0, false
}

// DCBlockerBank holds one independent DCBlocker per channel, matching Bank.
type DCBlockerBank struct {
	blockers []*DCBlocker
}

// NewDCBlockerBank returns a bank of channels independent blockers.
func NewDCBlockerBank(fc, fs float64, channels int) *DCBlockerBank {
	if channels <= 0 {
		panic("dsp: channels must be positive")
	}
	b := &DCBlockerBank{blockers: make([]*DCBlocker, channels)}
	for i := range b.blockers {
		b.blockers[i] = NewDCBlocker(fc, fs)
	}
	return b
}

// Process filters a channel-major chunk, returning new slices. It panics if
// the chunk's channel count does not match the bank's.
func (b *DCBlockerBank) Process(chunk [][]float64) [][]float64 {
	if len(chunk) != len(b.blockers) {
		panic("dsp: chunk channel count does not match bank")
	}
	out := make([][]float64, len(chunk))
	for i, ch := range chunk {
		out[i] = b.blockers[i].Process(ch)
	}
	return out
}

// Reset clears every channel's state.
func (b *DCBlockerBank) Reset() {
	for _, d := range b.blockers {
		d.Reset()
	}
}

// Channels returns the bank's channel count.
func (b *DCBlockerBank) Channels() int { return len(b.blockers) }
