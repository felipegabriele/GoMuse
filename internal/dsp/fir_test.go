package dsp

import (
	"math"
	"testing"
)

const (
	testFS   = 256.0
	testTaps = 32
	testLow  = 1.0
	testHigh = 40.0
)

func testKernel() []float64 {
	return FirwinBandpass(testTaps, testLow, testHigh, testFS)
}

// sine generates n samples of a unit-amplitude sine at freq Hz.
func sine(n int, freq, fs float64) []float64 {
	x := make([]float64, n)
	for i := range x {
		x[i] = math.Sin(2 * math.Pi * freq * float64(i) / fs)
	}
	return x
}

// amplitude estimates the amplitude of the freq component of x by projecting
// onto a quadrature pair. Phase-insensitive, so the filter's group delay does
// not matter.
func amplitude(x []float64, freq, fs float64) float64 {
	var re, im float64
	for i, v := range x {
		ph := 2 * math.Pi * freq * float64(i) / fs
		re += v * math.Cos(ph)
		im += v * math.Sin(ph)
	}
	n := float64(len(x))
	return 2 * math.Hypot(re, im) / n
}

func TestFirwinBandpassLengthAndSymmetry(t *testing.T) {
	b := testKernel()
	if len(b) != testTaps {
		t.Fatalf("len(b) = %d, want %d", len(b), testTaps)
	}
	for i := 0; i < len(b)/2; i++ {
		j := len(b) - 1 - i
		if math.Abs(b[i]-b[j]) > 1e-12 {
			t.Errorf("kernel not symmetric: b[%d] = %g, b[%d] = %g", i, b[i], j, b[j])
		}
	}
}

// Golden values from scipy:
//
//	firwin(32, np.array([1, 40]) / 128., pass_zero=False)
//
// This pins the port to its reference implementation.
func TestFirwinBandpassMatchesScipy(t *testing.T) {
	b := testKernel()
	want := []float64{
		0.00016412314706703443,
		0.001267248497524187,
		0.0008531577367000795,
		-0.0024457250624380145,
	}
	for i, w := range want {
		if math.Abs(b[i]-w) > 1e-15 {
			t.Errorf("b[%d] = %.17g, scipy gives %.17g", i, b[i], w)
		}
	}
	var sum float64
	for _, v := range b {
		sum += v
	}
	if math.Abs(sum-0.8702534130133917) > 1e-12 {
		t.Errorf("sum(b) = %.17g, scipy gives 0.8702534130133917", sum)
	}
}

// The DC gain of this kernel is 0.87, NOT ~0. That is not a porting bug -- it
// is what scipy produces for these arguments, and it is unavoidable: a 32-tap
// Hamming design at 256 Hz has a transition width of roughly 3.3*fs/N = 26 Hz,
// so a 1 Hz high-pass edge is nowhere near resolvable. muse-lsl's display
// filter simply does not remove DC or drift. See the package comment.
func TestFirwinBandpassDCGainIsNotZero(t *testing.T) {
	b := testKernel()
	var sum float64
	for _, v := range b {
		sum += v
	}
	if math.Abs(sum-0.8703) > 1e-3 {
		t.Errorf("DC gain = %g, want ~0.8703 (documented behaviour of the port)", sum)
	}
}

// Unity gain at the centre of the passband is what the scipy scaling buys us.
func TestFirwinBandpassUnityGainAtBandCentre(t *testing.T) {
	b := testKernel()
	fc := (testLow + testHigh) / 2
	m := float64(len(b)-1) / 2
	var g float64
	for n, v := range b {
		g += v * math.Cos(2*math.Pi*(float64(n)-m)*fc/testFS)
	}
	if math.Abs(g-1) > 1e-9 {
		t.Errorf("gain at %g Hz = %g, want 1", fc, g)
	}
}

// A constant input passes through at the kernel's DC gain -- and, crucially,
// it does so from the very first sample, with no transient. That settling
// behaviour is what the first-sample state priming buys us; the residual 87.0
// is the filter's honest DC gain, not a startup artifact.
func TestFilterDCIsSettledButNotRejected(t *testing.T) {
	f := NewFilter(testKernel())
	x := make([]float64, 1024)
	for i := range x {
		x[i] = 100.0
	}
	y := f.Process(x)

	// Perfectly flat everywhere, including the head: no ringing.
	lo, hi := y[0], y[0]
	for _, v := range y {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if hi-lo > 1e-9 {
		t.Errorf("startup transient: output spans [%g, %g], want flat", lo, hi)
	}

	// Tail sits at 100 * sum(b).
	tail := y[len(y)-512:]
	for _, v := range tail {
		if math.Abs(v-87.02534130133917) > 1e-9 {
			t.Fatalf("DC output = %g, want 87.0253 (= 100 * DC gain)", v)
		}
	}
}

// Without priming, a cold start would ring for the first numtaps-1 samples.
// Compare the primed head against the steady state to show it does not.
func TestFilterNoStartupTransientOnAStep(t *testing.T) {
	f := NewFilter(testKernel())
	x := sine(4096, 10, testFS)
	// Offset the whole signal by a large DC pedestal, as real EEG is.
	for i := range x {
		x[i] += 800
	}
	y := f.Process(x)

	// Steady-state amplitude of the 10 Hz component.
	want := amplitude(y[2048:], 10, testFS)
	got := amplitude(y[:1024], 10, testFS)
	if math.Abs(got-want) > 0.02 {
		t.Errorf("head amplitude %.4f differs from steady state %.4f: startup transient", got, want)
	}
}

// response evaluates |H(f)| of a kernel directly.
func response(b []float64, freq, fs float64) float64 {
	var re, im float64
	for n, v := range b {
		ph := 2 * math.Pi * freq * float64(n) / fs
		re += v * math.Cos(ph)
		im -= v * math.Sin(ph)
	}
	return math.Hypot(re, im)
}

// The full magnitude response, pinned to the values scipy produces. These are
// the real numbers this filter delivers -- note how gentle the low edge is.
func TestFilterMagnitudeResponse(t *testing.T) {
	b := testKernel()
	cases := []struct{ freq, want float64 }{
		{0, 0.870253},   // DC: essentially unattenuated
		{0.2, 0.870307}, // drift: unattenuated
		{1, 0.871597},   // nominal low edge, but only -1.2 dB
		{5, 0.900800},
		{10, 0.960929}, // passband
		{20, 1.000158}, // band centre, unity by construction
		{30, 0.960578}, // passband
		{40, 0.498314}, // nominal high edge, -6 dB as the convention has it
		{45, 0.202626},
		{50, 0.038725},  // 26x
		{60, 0.000821},  // 1218x -- mains hum is gone
		{80, 0.000580},  // 1724x
		{120, 0.000080}, // 12500x
		{128, 0.000000}, // Nyquist: exact null (even-length type II FIR)
	}
	for _, tc := range cases {
		got := response(b, tc.freq, testFS)
		if math.Abs(got-tc.want) > 1e-5 {
			t.Errorf("|H(%.1f Hz)| = %.6f, want %.6f", tc.freq, got, tc.want)
		}
	}
}

// The passband is flat to within 4% between 10 and 30 Hz.
func TestFilterPassband(t *testing.T) {
	for _, freq := range []float64{10, 20, 30} {
		f := NewFilter(testKernel())
		x := sine(8192, freq, testFS)
		y := f.Process(x)
		got := amplitude(y[2048:], freq, testFS)
		if got < 0.96 || got > 1.04 {
			t.Errorf("%.0f Hz: amplitude = %.4f, want within 4%% of 1.0", freq, got)
		}
	}
}

// Above the band the 32-tap design does very well.
func TestFilterStopbandHigh(t *testing.T) {
	cases := []struct {
		freq   float64
		maxAmp float64 // asserted bound
	}{
		{50, 0.045},   // measured 0.0387 -> 26x
		{60, 0.0012},  // measured 0.00082 -> 1218x, comfortably past the 10x asked for
		{80, 0.0008},  // measured 0.00058 -> 1724x
		{120, 0.0002}, // measured 0.00008 -> 12500x
	}
	for _, tc := range cases {
		f := NewFilter(testKernel())
		x := sine(8192, tc.freq, testFS)
		y := f.Process(x)
		got := amplitude(y[2048:], tc.freq, testFS)
		if got > tc.maxAmp {
			t.Errorf("%.0f Hz: amplitude = %.5f, want <= %.5f", tc.freq, got, tc.maxAmp)
		} else {
			t.Logf("%.0f Hz: amplitude = %.5f (%.0fx attenuation)", tc.freq, got, 1/got)
		}
	}
}

// Below the band it does essentially nothing, and this test says so out loud.
// A 0.2 Hz sine -- exactly the electrode drift a display filter is supposed to
// suppress -- comes through at 87% of its amplitude. Asserting a 10x rejection
// here would be asserting a fiction: 3.3*fs/numtaps = 26 Hz of transition width
// means a 1 Hz corner cannot exist in 32 taps at 256 Hz. Fixing it needs more
// taps (and more delay) or a separate DC blocker; see the package comment.
func TestFilterDoesNotRejectLowFrequencies(t *testing.T) {
	for _, freq := range []float64{0.2, 0.5, 1} {
		f := NewFilter(testKernel())
		// Several periods even at 0.2 Hz (period = 1280 samples).
		x := sine(65536, freq, testFS)
		y := f.Process(x)
		got := amplitude(y[2048:], freq, testFS)
		if got < 0.85 || got > 0.90 {
			t.Errorf("%.1f Hz: amplitude = %.4f, want ~0.87 (documented: no low-end rejection)", freq, got)
		}
		t.Logf("%.1f Hz: amplitude = %.4f (only %.2fx attenuation)", freq, got, 1/got)
	}
}

func TestFilterContinuityAcrossChunks(t *testing.T) {
	taps := testKernel()
	x := make([]float64, 1000)
	for i := range x {
		x[i] = math.Sin(float64(i)*0.11) + 0.5*math.Cos(float64(i)*0.7) + 3
	}

	whole := NewFilter(taps).Process(x)

	chunked := make([]float64, 0, len(x))
	f := NewFilter(taps)
	for _, size := range []int{1, 2, 3, 5, 12, 12, 12, 31, 32, 33, 100, 257} {
		if len(chunked)+size > len(x) {
			size = len(x) - len(chunked)
		}
		if size == 0 {
			break
		}
		chunked = append(chunked, f.Process(x[len(chunked):len(chunked)+size])...)
	}
	// Drain the remainder in one go.
	if len(chunked) < len(x) {
		chunked = append(chunked, f.Process(x[len(chunked):])...)
	}

	if len(chunked) != len(whole) {
		t.Fatalf("chunked length %d, want %d", len(chunked), len(whole))
	}
	for i := range whole {
		if math.Abs(whole[i]-chunked[i]) > 1e-12 {
			t.Fatalf("sample %d: whole = %.15g, chunked = %.15g", i, whole[i], chunked[i])
		}
	}
}

func TestFilterDoesNotModifyInput(t *testing.T) {
	f := NewFilter(testKernel())
	x := sine(64, 10, testFS)
	orig := make([]float64, len(x))
	copy(orig, x)
	f.Process(x)
	for i := range x {
		if x[i] != orig[i] {
			t.Fatalf("Process modified input at %d", i)
		}
	}
}

func TestFilterReset(t *testing.T) {
	taps := testKernel()
	x := sine(200, 10, testFS)

	a := NewFilter(taps)
	first := a.Process(x)
	a.Reset()
	second := a.Process(x)

	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("after Reset, sample %d differs: %g vs %g", i, first[i], second[i])
		}
	}
}

func TestFilterEmptyInput(t *testing.T) {
	f := NewFilter(testKernel())
	if got := f.Process(nil); len(got) != 0 {
		t.Errorf("Process(nil) returned %d samples", len(got))
	}
	// An empty first call must not prime the state; the next real call should
	// still behave exactly like a fresh filter.
	x := sine(64, 10, testFS)
	got := f.Process(x)
	want := NewFilter(testKernel()).Process(x)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("an empty call disturbed the state at sample %d: %g vs %g", i, got[i], want[i])
		}
	}
}

func TestBankChannelIndependence(t *testing.T) {
	taps := testKernel()
	const n = 2048

	ch0 := sine(n, 10, testFS) // passband
	ch1 := sine(n, 60, testFS) // stopband
	ch2 := make([]float64, n)  // constant: pure DC
	for i := range ch2 {
		ch2[i] = 500
	}

	// Reference: each channel through its own standalone filter.
	want0 := NewFilter(taps).Process(ch0)
	want1 := NewFilter(taps).Process(ch1)
	want2 := NewFilter(taps).Process(ch2)

	b := NewBank(taps, 3)
	// Feed it in chunks, to exercise per-channel state as well.
	got := [][]float64{{}, {}, {}}
	for off := 0; off < n; off += 12 {
		end := off + 12
		if end > n {
			end = n
		}
		out := b.Process([][]float64{ch0[off:end], ch1[off:end], ch2[off:end]})
		for c := range out {
			got[c] = append(got[c], out[c]...)
		}
	}

	for c, want := range [][]float64{want0, want1, want2} {
		if len(got[c]) != len(want) {
			t.Fatalf("channel %d: got %d samples, want %d", c, len(got[c]), len(want))
		}
		for i := range want {
			if math.Abs(got[c][i]-want[i]) > 1e-12 {
				t.Fatalf("channel %d sample %d: got %.15g, want %.15g", c, i, got[c][i], want[i])
			}
		}
	}
}

func TestBankResetAndChannels(t *testing.T) {
	b := NewBank(testKernel(), 5)
	if b.Channels() != 5 {
		t.Fatalf("Channels() = %d, want 5", b.Channels())
	}
	x := [][]float64{}
	for i := 0; i < 5; i++ {
		x = append(x, sine(100, 10, testFS))
	}
	first := b.Process(x)
	b.Reset()
	second := b.Process(x)
	for c := range first {
		for i := range first[c] {
			if first[c][i] != second[c][i] {
				t.Fatalf("after Reset, channel %d sample %d differs", c, i)
			}
		}
	}
}

func TestNewFilterPanicsOnEmptyTaps(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewFilter(nil) did not panic")
		}
	}()
	NewFilter(nil)
}

func TestBankPanicsOnChannelMismatch(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Bank.Process with wrong channel count did not panic")
		}
	}()
	b := NewBank(testKernel(), 4)
	b.Process([][]float64{{1, 2, 3}})
}

func TestFirwinBandpassPanics(t *testing.T) {
	cases := []struct {
		name          string
		taps          int
		low, high, fs float64
	}{
		{"zero taps", 0, 1, 40, 256},
		{"low >= high", 32, 40, 40, 256},
		{"low <= 0", 32, 0, 40, 256},
		{"high above nyquist", 32, 1, 200, 256},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected panic")
				}
			}()
			FirwinBandpass(tc.taps, tc.low, tc.high, tc.fs)
		})
	}
}
