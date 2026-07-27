// Package dsp provides the small amount of signal processing gomuse needs for
// display: a windowed-sinc FIR bandpass and a stateful filter that can be run
// over a stream in chunks without seams.
//
// It is a port of the display filter in muse-lsl's viewer_v1.py, which builds a
// 32-tap 1-40 Hz bandpass with scipy's firwin and applies it with
// scipy.signal.lfilter, carrying the filter state (zi) across plot updates.
//
// Deliberate simplification: scipy's firwin switches to a Kaiser window when a
// `width` argument is given, and muse-lsl passes width=0.05. We use firwin's
// default Hamming window instead. FirwinBandpass is otherwise exact: it
// reproduces scipy's firwin(32, [1, 40]/128., pass_zero=False) to within 1e-15
// per tap, which the tests pin against golden values.
//
// # What this filter actually does
//
// The name "1-40 Hz bandpass" oversells it. A Hamming design has a transition
// width of roughly 3.3*fs/numtaps, which at 32 taps and 256 Hz is about 26 Hz.
// The 40 Hz edge is therefore real, but the 1 Hz edge is not: it is far narrower
// than the filter can resolve, so the low side never rolls off. The measured
// magnitude response is
//
//	  0 Hz   0.870      40 Hz   0.498  (the -6 dB point, as the convention has it)
//	0.2 Hz   0.870      50 Hz   0.039  (26x)
//	  1 Hz   0.872      60 Hz   0.00082 (1218x)
//	  5 Hz   0.901      80 Hz   0.00058 (1724x)
//	 10 Hz   0.961     120 Hz   0.00008 (12500x)
//	 20 Hz   1.000     128 Hz   0       (exact null at Nyquist)
//
// So it is really a lowpass with a mild low-frequency tilt: excellent at
// removing mains hum and EMG, and almost useless against DC offset and
// electrode drift, which pass at 87% of their amplitude. This is faithful to
// muse-lsl -- viewer_v1.py leans on its autoscaling plot to hide the offset
// rather than on the filter.
//
// Getting a genuine 1 Hz corner needs many more taps, and the group delay that
// comes with them ((numtaps-1)/2 samples). For reference, at 256 Hz:
//
//	numtaps  band     delay    gain at DC  1 Hz   5 Hz   10 Hz
//	     32  1-40 Hz  0.06 s   0.870       0.872  0.901  0.961
//	    128  1-40 Hz  0.25 s   0.497       0.581  1.003  1.003
//	    512  1-40 Hz  1.00 s   0.004       0.501  0.999  0.999
//	     64  5-40 Hz  0.12 s   0.070       0.092  0.501  0.967
//	    128  5-40 Hz  0.25 s   0.003       0.003  0.498  1.000
//
// If a caller needs real drift rejection at low latency, a one-pole DC blocker
// in front of this filter is the cheaper answer than a longer kernel.
package dsp

import "math"

// sinc is the normalized cardinal sine, sin(pi*x)/(pi*x), with sinc(0) = 1.
func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

// FirwinBandpass designs a numtaps-long linear-phase FIR bandpass kernel by the
// windowed-sinc method, equivalent to scipy.signal.firwin(numtaps, [low, high],
// fs=fs, pass_zero=False) with the default Hamming window.
//
// low and high are the band edges in Hz and fs is the sample rate in Hz; they
// must satisfy 0 < low < high < fs/2. The kernel is scaled for unity gain at
// the centre of the passband, (low+high)/2, matching scipy's normalisation.
//
// It panics on invalid arguments, since the filter is designed once at startup
// from constants.
func FirwinBandpass(numtaps int, low, high, fs float64) []float64 {
	if numtaps < 1 {
		panic("dsp: numtaps must be >= 1")
	}
	if fs <= 0 {
		panic("dsp: fs must be > 0")
	}
	if !(low > 0 && low < high && high < fs/2) {
		panic("dsp: require 0 < low < high < fs/2")
	}

	// Normalized cutoffs in cycles per sample.
	f1 := low / fs
	f2 := high / fs

	b := make([]float64, numtaps)
	m := float64(numtaps-1) / 2 // centre of symmetry; half-integer when numtaps is even

	for n := range b {
		d := float64(n) - m
		// Difference of two ideal lowpass kernels.
		h := 2*f2*sinc(2*f2*d) - 2*f1*sinc(2*f1*d)
		// Hamming window (symmetric form, as used by scipy's firwin).
		w := 0.54 - 0.46*math.Cos(2*math.Pi*float64(n)/float64(numtaps-1))
		if numtaps == 1 {
			w = 1
		}
		b[n] = h * w
	}

	// Normalise to unity gain at the passband centre. The kernel is symmetric
	// about m, so its response there is real and equal to sum(b[n]*cos(...)).
	fc := (f1 + f2) / 2
	var gain float64
	for n, v := range b {
		gain += v * math.Cos(2*math.Pi*(float64(n)-m)*fc)
	}
	if gain == 0 {
		panic("dsp: degenerate filter design (zero gain at band centre)")
	}
	for n := range b {
		b[n] /= gain
	}
	return b
}

// Filter applies a fixed FIR kernel to a stream of samples, carrying the last
// numtaps-1 inputs across calls so that consecutive chunks join seamlessly.
//
// A Filter is not safe for concurrent use; give each client (and each channel)
// its own.
type Filter struct {
	taps []float64
	// hist holds the previous len(taps)-1 input samples in chronological
	// order: hist[len(hist)-1] is the most recent.
	hist []float64
	// primed reports whether hist has been initialised from the first sample.
	primed bool
	// buf is scratch reused across Process calls.
	buf []float64
}

// NewFilter returns a Filter using the given kernel. The kernel is copied, so
// the caller may reuse or modify its slice. It panics on an empty kernel.
func NewFilter(taps []float64) *Filter {
	if len(taps) == 0 {
		panic("dsp: filter needs at least one tap")
	}
	t := make([]float64, len(taps))
	copy(t, taps)
	return &Filter{
		taps: t,
		hist: make([]float64, len(t)-1),
	}
}

// NumTaps returns the length of the kernel.
func (f *Filter) NumTaps() int { return len(f.taps) }

// Process filters x and returns a newly allocated slice of the same length. The
// input is not modified, and the returned slice is owned by the caller.
//
// On the first call after construction or Reset, the filter state is
// initialised from x[0] rather than from zero. This is scipy's
// lfilter_zi-scaled-by-the-first-sample idiom: it starts the filter in the
// steady state it would reach on a constant x[0], so a stream that begins at a
// large DC offset -- as EEG always does -- does not open with a 32-sample
// ringing artifact.
func (f *Filter) Process(x []float64) []float64 {
	if len(x) == 0 {
		return []float64{}
	}
	if !f.primed {
		for i := range f.hist {
			f.hist[i] = x[0]
		}
		f.primed = true
	}

	n := len(f.taps)
	// buf = hist ++ x, so buf[i+n-1] is x[i] and buf[i+n-1-k] is x[i-k].
	need := len(f.hist) + len(x)
	if cap(f.buf) < need {
		f.buf = make([]float64, need)
	}
	buf := f.buf[:need]
	copy(buf, f.hist)
	copy(buf[len(f.hist):], x)

	y := make([]float64, len(x))
	for i := range y {
		var acc float64
		base := i + n - 1
		for k, b := range f.taps {
			acc += b * buf[base-k]
		}
		y[i] = acc
	}

	// Carry the trailing n-1 inputs into the next call.
	copy(f.hist, buf[need-len(f.hist):])
	return y
}

// Reset discards the filter state, so the next Process call starts fresh and
// re-primes from its first sample.
func (f *Filter) Reset() {
	f.primed = false
	for i := range f.hist {
		f.hist[i] = 0
	}
}

// Bank holds one independent Filter per channel, all sharing the same kernel.
// Each WebSocket client needs its own Bank, since the display filter is
// toggleable per client.
//
// A Bank is not safe for concurrent use.
type Bank struct {
	filters []*Filter
}

// NewBank returns a Bank of channels independent filters over the given kernel.
// It panics if channels is negative or the kernel is empty.
func NewBank(taps []float64, channels int) *Bank {
	if channels < 0 {
		panic("dsp: channels must be >= 0")
	}
	fs := make([]*Filter, channels)
	for i := range fs {
		fs[i] = NewFilter(taps)
	}
	return &Bank{filters: fs}
}

// Channels returns the number of channels the Bank was built for.
func (b *Bank) Channels() int { return len(b.filters) }

// Process filters a channel-major chunk (chunk[c][i] is sample i of channel c)
// and returns a newly allocated chunk of the same shape. The input is not
// modified. It panics if the chunk's channel count does not match the Bank's.
func (b *Bank) Process(chunk [][]float64) [][]float64 {
	if len(chunk) != len(b.filters) {
		panic("dsp: chunk channel count does not match bank")
	}
	out := make([][]float64, len(chunk))
	for c, ch := range chunk {
		out[c] = b.filters[c].Process(ch)
	}
	return out
}

// Reset clears every channel's state.
func (b *Bank) Reset() {
	for _, f := range b.filters {
		f.Reset()
	}
}
