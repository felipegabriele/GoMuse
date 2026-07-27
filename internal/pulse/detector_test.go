package pulse

import (
	"math"
	"testing"
)

// pulseTrain builds a PPG-like waveform: a large DC pedestal with a periodic
// asymmetric bump, matching the shape internal/synth generates.
func pulseTrain(n int, rate, bpm, pedestal, modulation float64) (samples, ts []float64) {
	samples = make([]float64, n)
	ts = make([]float64, n)
	hz := bpm / 60
	for i := range samples {
		e := float64(i) / rate
		phase := math.Mod(e*hz, 1)
		// A sharp systolic rise and slower decay, peaking early in the cycle.
		bump := math.Exp(-(phase - 0.2) / 0.12 * ((phase - 0.2) / 0.12))
		samples[i] = pedestal * (1 + modulation*bump)
		ts[i] = 1000 + e
	}
	return samples, ts
}

func TestDetectorFindsKnownRate(t *testing.T) {
	const rate, bpm = 64.0, 72.0
	samples, ts := pulseTrain(64*20, rate, bpm, 100000, 0.04) // 20 s

	d := NewDetector(rate)
	beats, gap := d.Process(samples, ts)
	if gap {
		t.Error("gap reported on a continuous stream")
	}

	// 20 s at 72 bpm is 24 beats. Allow the first to be missed while the peak
	// estimate settles.
	if len(beats) < 22 || len(beats) > 25 {
		t.Fatalf("detected %d beats over 20 s at 72 bpm, want 22-25", len(beats))
	}

	// Mean interval should land within 2 bpm of the truth.
	span := beats[len(beats)-1] - beats[0]
	got := 60 * float64(len(beats)-1) / span
	if math.Abs(got-bpm) > 2 {
		t.Errorf("implied rate = %.1f bpm, want %.0f +/- 2", got, bpm)
	}
}

func TestDetectorRefractoryRejectsDoublePeaks(t *testing.T) {
	const rate = 64.0
	// A dicrotic notch: a second, smaller bump 100 ms after each beat, well
	// inside the refractory period. Real PPG has one; counting it doubles the
	// reported rate.
	n := int(rate * 10)
	samples := make([]float64, n)
	ts := make([]float64, n)
	for i := range samples {
		e := float64(i) / rate
		phase := math.Mod(e*1.2, 1) // 72 bpm
		main := math.Exp(-(phase - 0.2) / 0.10 * ((phase - 0.2) / 0.10))
		notch := 0.5 * math.Exp(-(phase-0.32)/0.06*((phase-0.32)/0.06))
		samples[i] = 100000 * (1 + 0.04*(main+notch))
		ts[i] = 1000 + e
	}

	d := NewDetector(rate)
	beats, _ := d.Process(samples, ts)

	// 10 s at 72 bpm is 12 beats. Counting notches would give ~24.
	if len(beats) > 14 {
		t.Errorf("detected %d beats, want <=14: the notch is being counted", len(beats))
	}
}

func TestDetectorReportsGap(t *testing.T) {
	const rate = 64.0
	samples, ts := pulseTrain(int(rate*5), rate, 72, 100000, 0.04)

	d := NewDetector(rate)
	if _, gap := d.Process(samples, ts); gap {
		t.Error("gap reported on the first block")
	}

	// A second block starting 2 s after the first ended: samples were lost.
	next, nextTS := pulseTrain(int(rate*5), rate, 72, 100000, 0.04)
	last := ts[len(ts)-1]
	for i := range nextTS {
		nextTS[i] = last + 2.0 + float64(i)/rate
	}
	if _, gap := d.Process(next, nextTS); !gap {
		t.Error("no gap reported across a 2 s discontinuity")
	}
}

func TestDetectorContiguousBlocksReportNoGap(t *testing.T) {
	const rate = 64.0
	samples, ts := pulseTrain(int(rate*10), rate, 72, 100000, 0.04)

	d := NewDetector(rate)
	// Feed in 16-sample blocks, as real chunks arrive.
	for i := 0; i+16 <= len(samples); i += 16 {
		if _, gap := d.Process(samples[i:i+16], ts[i:i+16]); gap {
			t.Fatalf("gap reported at contiguous block starting %d", i)
		}
	}
}
