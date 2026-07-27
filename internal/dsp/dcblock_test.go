package dsp

import (
	"math"
	"testing"
)

// sine and amplitude are shared with fir_test.go.

// TestDCBlockerRemovesConstant: a constant input must go to zero, which is the
// whole point and precisely what the 32-tap FIR fails to do.
func TestDCBlockerRemovesConstant(t *testing.T) {
	d := NewDCBlocker(0.5, 256)
	x := make([]float64, 1024)
	for i := range x {
		x[i] = 100
	}
	y := d.Process(x)

	// Priming means even the first sample is settled, so check the whole run.
	for i, v := range y {
		if math.Abs(v) > 1e-9 {
			t.Fatalf("sample %d = %v, want ~0", i, v)
		}
	}
}

// TestDCBlockerNoStartupTransient: without priming, a step equal to the first
// sample would ring for seconds and be plainly visible on the trace.
func TestDCBlockerNoStartupTransient(t *testing.T) {
	d := NewDCBlocker(0.5, 256)
	x := make([]float64, 64)
	for i := range x {
		x[i] = 500 // a large offset, as a drifting electrode produces
	}
	y := d.Process(x)
	if math.Abs(y[0]) > 1e-9 {
		t.Errorf("first output = %v, want ~0 (filter should start settled)", y[0])
	}
}

// TestDCBlockerPassesEEGBand: the blocker must be nearly transparent across
// the frequencies anyone actually looks at, or it is doing harm.
func TestDCBlockerPassesEEGBand(t *testing.T) {
	const fs = 256
	for _, freq := range []float64{4, 8, 10, 20, 40} {
		d := NewDCBlocker(0.5, fs)
		got := amplitude(d.Process(sine(4096, freq, fs)), freq, fs)
		if got < 0.99 || got > 1.01 {
			t.Errorf("%.0f Hz amplitude = %.4f, want within 1%% of 1.0", freq, got)
		}
	}
}

// TestDCBlockerAttenuatesDrift pins the real numbers at the frequencies the
// FIR alone leaves untouched (it passes 0.2 Hz at ~0.87).
func TestDCBlockerAttenuatesDrift(t *testing.T) {
	const fs = 256
	cases := []struct {
		freq    float64
		wantMax float64
	}{
		{0.05, 0.15},
		{0.1, 0.30},
		{0.2, 0.55},
	}
	for _, c := range cases {
		d := NewDCBlocker(0.5, fs)
		got := amplitude(d.Process(sine(1<<16, c.freq, fs)), c.freq, fs)
		if got > c.wantMax {
			t.Errorf("%.2f Hz amplitude = %.4f, want <= %.2f", c.freq, got, c.wantMax)
		}
		if got <= 0 {
			t.Errorf("%.2f Hz amplitude = %v, expected a positive value", c.freq, got)
		}
	}
}

// TestDCBlockerChunkContinuity: filtering in pieces must equal filtering whole,
// or every chunk boundary becomes a visible discontinuity.
func TestDCBlockerChunkContinuity(t *testing.T) {
	const fs = 256
	x := sine(1000, 7, fs)
	for i := range x {
		x[i] += 50 // ride it on an offset so the DC path is exercised too
	}

	whole := NewDCBlocker(0.5, fs).Process(x)

	for _, size := range []int{1, 3, 12, 31, 100} {
		d := NewDCBlocker(0.5, fs)
		var pieces []float64
		for i := 0; i < len(x); i += size {
			end := min(i+size, len(x))
			pieces = append(pieces, d.Process(x[i:end])...)
		}
		if len(pieces) != len(whole) {
			t.Fatalf("chunk %d: length %d, want %d", size, len(pieces), len(whole))
		}
		for i := range whole {
			if math.Abs(pieces[i]-whole[i]) > 1e-12 {
				t.Fatalf("chunk %d: sample %d = %v, want %v", size, i, pieces[i], whole[i])
			}
		}
	}
}

// TestDCBlockerProcessDoesNotMutateInput guards the callers that publish the
// same chunk to several subscribers.
func TestDCBlockerProcessDoesNotMutateInput(t *testing.T) {
	d := NewDCBlocker(0.5, 256)
	x := []float64{1, 2, 3, 4, 5}
	orig := append([]float64(nil), x...)
	d.Process(x)
	for i := range x {
		if x[i] != orig[i] {
			t.Fatalf("input mutated at %d: %v != %v", i, x[i], orig[i])
		}
	}
}

// TestDCBlockerBankIndependence: channels must not bleed into one another.
func TestDCBlockerBankIndependence(t *testing.T) {
	const fs = 256
	b := NewDCBlockerBank(0.5, fs, 3)

	chunk := [][]float64{
		sine(512, 10, fs),
		make([]float64, 512), // silent
		sine(512, 20, fs),
	}
	for i := range chunk[0] {
		chunk[1][i] = 300 // constant offset only
	}

	var out [][]float64
	for i := 0; i < 512; i += 12 {
		end := min(i+12, 512)
		piece := [][]float64{chunk[0][i:end], chunk[1][i:end], chunk[2][i:end]}
		res := b.Process(piece)
		if out == nil {
			out = make([][]float64, 3)
		}
		for c := range res {
			out[c] = append(out[c], res[c]...)
		}
	}

	// The silent-but-offset channel must settle to zero regardless of what
	// its neighbours are doing.
	for i, v := range out[1] {
		if math.Abs(v) > 1e-9 {
			t.Fatalf("silent channel sample %d = %v, want ~0", i, v)
		}
	}
	if got := amplitude(out[0], 10, fs); got < 0.99 || got > 1.01 {
		t.Errorf("channel 0 amplitude = %.4f, want ~1.0", got)
	}
	if got := amplitude(out[2], 20, fs); got < 0.99 || got > 1.01 {
		t.Errorf("channel 2 amplitude = %.4f, want ~1.0", got)
	}
}

// TestDCBlockerRejectsBadParams: misconfiguration is a programming error.
func TestDCBlockerRejectsBadParams(t *testing.T) {
	cases := []struct{ fc, fs float64 }{
		{0.5, 0}, {0, 256}, {-1, 256}, {128, 256}, {200, 256},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewDCBlocker(%v, %v) did not panic", c.fc, c.fs)
				}
			}()
			NewDCBlocker(c.fc, c.fs)
		}()
	}
}
