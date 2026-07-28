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

// The refractory period, and only the refractory period, must reject the
// dicrotic notch.
//
// The notch here is deliberately tall — 0.9 of the systolic peak — so that after
// smoothing it still clears PeakFraction of the running peak estimate. An
// earlier, shorter notch was rejected by the *amplitude* threshold instead, so
// the test passed with the refractory condition replaced by a bare isPeak: it
// named a guard it did not exercise. On this waveform the minimum gap the
// refractory check actually has to reject is the notch's 100 ms lag.
func TestDetectorRefractoryRejectsDoublePeaks(t *testing.T) {
	const rate = 64.0
	n := int(rate * 10)
	samples := make([]float64, n)
	ts := make([]float64, n)
	for i := range samples {
		e := float64(i) / rate
		phase := math.Mod(e*1.2, 1) // 72 bpm
		main := math.Exp(-(phase - 0.2) / 0.06 * ((phase - 0.2) / 0.06))
		notch := 0.9 * math.Exp(-(phase-0.32)/0.045*((phase-0.32)/0.045))
		samples[i] = 100000 * (1 + 0.04*(main+notch))
		ts[i] = 1000 + e
	}

	d := NewDetector(rate)
	beats, _ := d.Process(samples, ts)

	// 10 s at 72 bpm is 12 beats. Counting notches gives ~24.
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

// The two hole thresholds must stay genuinely separate, in both directions.
// Welding them together is what stopped the feature locking under routine
// sample loss, so a hole that breaks the interval chain must still be able to
// leave the filter and peak state alone.
//
// The signal state after a reset is compared against a brand-new detector fed
// the same post-hole samples, the same technique
// TestTrackerResetsOnChannelSwitch uses.
func TestDetectorResetsSignalStateOnlyForLongHoles(t *testing.T) {
	const rate = 64.0
	const tail = 4 // post-hole samples: fewer than SmoothTaps, so state is easy to compare

	after := func(holeSec float64) *Detector {
		samples, ts := pulseTrain(int(rate*10), rate, 72, 100000, 0.04)
		d := NewDetector(rate)
		d.Process(samples, ts)

		next, nts := pulseTrain(tail, rate, 72, 100000, 0.04)
		for i := range nts {
			nts[i] = ts[len(ts)-1] + holeSec + float64(i+1)/rate
		}
		if _, gap := d.Process(next, nts); !gap {
			t.Fatalf("hole of %.2f s reported no gap, want the chain broken", holeSec)
		}
		return d
	}

	// A detector that has only ever seen the post-hole samples.
	fresh := NewDetector(rate)
	next, nts := pulseTrain(tail, rate, 72, 100000, 0.04)
	fresh.Process(next, nts)

	// Just over ChainBreakSec but well under SignalResetSec: the chain breaks,
	// the ~3 s of peak adaptation and the DC blocker history are kept.
	short := after(0.1)
	if short.peak == fresh.peak {
		t.Errorf("peak after a 0.1 s hole = %g, same as a fresh detector: signal "+
			"state was reset for a hole under SignalResetSec=%.2f", short.peak, SignalResetSec)
	}

	// Well over SignalResetSec: the history describes a signal that no longer
	// continues, so it goes.
	long := after(2.0)
	if long.peak != fresh.peak || long.have != fresh.have {
		t.Errorf("state after a 2 s hole = {have:%d peak:%g}, want a fresh detector's "+
			"{have:%d peak:%g}: signal state was not reset above SignalResetSec=%.2f",
			long.have, long.peak, fresh.have, fresh.peak, SignalResetSec)
	}
}

func TestDetectorContiguousBlocksReportNoGap(t *testing.T) {
	const rate = 64.0
	samples, ts := pulseTrain(int(rate*10), rate, 72, 100000, 0.04)

	// A single whole-buffer call establishes the ground truth for how many
	// beats this waveform contains.
	whole := NewDetector(rate)
	wantBeats, _ := whole.Process(samples, ts)

	d := NewDetector(rate)
	var gotBeats []float64
	// Feed in 16-sample blocks, as real chunks arrive. A regression that
	// resets prev/prevPrev/have at the top of every Process call would break
	// mid-pulse detection at chunk boundaries while still reporting no gaps,
	// so the beat count -- not just the gap flag -- must match the
	// whole-buffer run.
	for i := 0; i+16 <= len(samples); i += 16 {
		beats, gap := d.Process(samples[i:i+16], ts[i:i+16])
		if gap {
			t.Fatalf("gap reported at contiguous block starting %d", i)
		}
		gotBeats = append(gotBeats, beats...)
	}

	if len(gotBeats) != len(wantBeats) {
		t.Fatalf("chunked processing found %d beats, whole-buffer processing found %d: cross-call state was not preserved", len(gotBeats), len(wantBeats))
	}
	for i := range wantBeats {
		if gotBeats[i] != wantBeats[i] {
			t.Fatalf("beat %d differs: chunked %.6f vs whole-buffer %.6f", i, gotBeats[i], wantBeats[i])
		}
	}
}

// TestDetectorFirstBeatUnderRefractoryWindow pins the first-beat guarantee to
// whether a beat has actually been accepted, not to whether Process has run.
//
// The first block here is deliberately shorter than one pulse period, so
// Process runs to completion without finding a beat. If the refractory check
// were gated on "Process has run before" rather than "a beat has been
// accepted", the second block's genuine first beat -- which lands well under
// RefractorySec on this zero-based clock -- would be measured against a
// phantom beat at t=0 and rejected.
func TestDetectorFirstBeatUnderRefractoryWindow(t *testing.T) {
	const rate, bpm = 64.0, 72.0
	hz := bpm / 60
	n := int(rate * 2)
	samples := make([]float64, n)
	ts := make([]float64, n)
	for i := range samples {
		e := float64(i) / rate
		phase := math.Mod(e*hz, 1)
		bump := math.Exp(-(phase - 0.2) / 0.12 * ((phase - 0.2) / 0.12))
		samples[i] = 100000 * (1 + 0.04*bump)
		ts[i] = e // zero-based, unlike pulseTrain's 1000+e Unix-like offset
	}

	d := NewDetector(rate)

	// First block: 8 samples (~0.109 s), ending before the systolic peak at
	// ~0.167 s.
	beats1, gap1 := d.Process(samples[:8], ts[:8])
	if gap1 {
		t.Fatal("unexpected gap on first block")
	}
	if len(beats1) != 0 {
		t.Fatalf("first block already reports a beat (%v); test no longer exercises the no-beat-yet case", beats1)
	}

	beats2, gap2 := d.Process(samples[8:], ts[8:])
	if gap2 {
		t.Fatal("unexpected gap on second block")
	}
	// The true first beat sits at ~0.167 s, under RefractorySec. Gating
	// acceptance on "Process has run" instead of "a beat was accepted" rejects
	// it against a phantom beat at t=0 and only picks up the *second* real
	// beat, roughly one period (~0.833 s) later -- so this must check the
	// earliest beat's timing, not merely that some beat was found.
	if len(beats2) == 0 || beats2[0] >= RefractorySec {
		t.Fatalf("first true beat (expected near t=0.167s, under RefractorySec=%.3fs) was rejected by the refractory check against a phantom beat at t=0; got beats %v", RefractorySec, beats2)
	}
}
