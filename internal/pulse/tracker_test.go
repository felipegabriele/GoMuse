package pulse

import (
	"math"
	"testing"
)

// feed pushes a multi-channel block through the tracker in chunk-sized pieces,
// as the server does.
func feed(tr *Tracker, data [][]float64, ts []float64, chunk int) {
	for i := 0; i < len(ts); i += chunk {
		end := i + chunk
		if end > len(ts) {
			end = len(ts)
		}
		block := make([][]float64, len(data))
		for c := range data {
			block[c] = data[c][i:end]
		}
		tr.Process(block, ts[i:end])
	}
}

func TestTrackerLocksOntoPulseAndReportsRate(t *testing.T) {
	const rate, bpm = 64.0, 72.0
	pulseCh, ts := pulseTrain(int(rate*60), rate, bpm, 100000, 0.04)
	flat, _ := pulseTrain(int(rate*60), rate, bpm, 100000, 0.0002)

	tr := NewTracker(rate, 3)
	feed(tr, [][]float64{flat, pulseCh, flat}, ts, 16)

	snap := tr.Snapshot()
	if snap.State != StateLocked {
		t.Fatalf("State = %q, want %q", snap.State, StateLocked)
	}
	if snap.Channel != 1 {
		t.Errorf("Channel = %d, want 1 (the modulating channel)", snap.Channel)
	}
	if !snap.HasBPM {
		t.Fatal("HasBPM = false after 60 s of clean pulse")
	}
	if math.Abs(snap.BPM-bpm) > 3 {
		t.Errorf("BPM = %.1f, want %.0f +/- 3", snap.BPM, bpm)
	}
	if !snap.HasRMSSD {
		t.Error("HasRMSSD = false after 60 s, want RMSSD reported")
	}
	if snap.BeatCount < 60 {
		t.Errorf("BeatCount = %d, want >=60 over a minute at 72 bpm", snap.BeatCount)
	}
}

func TestTrackerReportsNoPulseOnFlatSignal(t *testing.T) {
	const rate = 64.0
	flat, ts := pulseTrain(int(rate*20), rate, 72, 100000, 0.0002)

	tr := NewTracker(rate, 2)
	feed(tr, [][]float64{flat, flat}, ts, 16)

	snap := tr.Snapshot()
	if snap.State != StateNoPulse {
		t.Errorf("State = %q, want %q on a signal below the modulation floor",
			snap.State, StateNoPulse)
	}
	if snap.HasBPM || snap.HasRMSSD {
		t.Error("numbers reported in the no-pulse state, want none")
	}
	if snap.Channel != -1 {
		t.Errorf("Channel = %d, want -1 when nothing qualifies", snap.Channel)
	}
}

func TestTrackerTransitionsFlatToLocked(t *testing.T) {
	const rate = 64.0
	tr := NewTracker(rate, 1)

	flat, ts := pulseTrain(int(rate*10), rate, 72, 100000, 0.0002)
	feed(tr, [][]float64{flat}, ts, 16)
	if got := tr.Snapshot().State; got != StateNoPulse {
		t.Fatalf("State = %q while flat, want %q", got, StateNoPulse)
	}

	beat, bts := pulseTrain(int(rate*60), rate, 72, 100000, 0.04)
	for i := range bts {
		bts[i] = ts[len(ts)-1] + float64(i+1)/rate
	}
	feed(tr, [][]float64{beat}, bts, 16)
	if got := tr.Snapshot().State; got != StateLocked {
		t.Errorf("State = %q once pulsing, want %q", got, StateLocked)
	}
}

// The Detector reports a gap; the Tracker must translate that into HRV.Break so
// no interval spans lost data. Tested here because it is the wiring between two
// units that are each already correct on their own.
func TestTrackerBreaksIntervalChainAcrossGap(t *testing.T) {
	const rate = 64.0
	first, ts := pulseTrain(int(rate*30), rate, 72, 100000, 0.04)

	tr := NewTracker(rate, 1)
	feed(tr, [][]float64{first}, ts, 16)
	before := tr.hrv.IntervalCount()
	if before < 5 {
		t.Fatalf("only %d intervals after 30 s, want >=5", before)
	}

	// A second run starting 5 s later: samples were lost in between.
	second, sts := pulseTrain(int(rate*30), rate, 72, 100000, 0.04)
	for i := range sts {
		sts[i] = ts[len(ts)-1] + 5.0 + float64(i)/rate
	}
	feed(tr, [][]float64{second}, sts, 16)

	// Every interval in the second run is legitimate, but none may span the
	// discontinuity. The beat closing the gap contributes no interval, so the
	// total is one short of what contiguous data would have produced.
	//
	// Compared against the HRV's own beat count rather than Snapshot's, since
	// Snapshot windows beats from the last raw sample timestamp while the HRV
	// windows its beats from the last accepted beat's own timestamp — those two
	// cutoffs differ by the detector's inherent detection lag and would make
	// this comparison flaky by +/-1 for reasons unrelated to the gap wiring.
	beats := len(tr.hrv.beats)
	if got := tr.hrv.IntervalCount(); got >= beats-1 {
		t.Errorf("IntervalCount = %d with %d beats: an interval spans the gap",
			got, beats)
	}
}

// A channel switch must not carry filter or beat history across, since the two
// channels have different pedestals and unrelated phase.
func TestTrackerResetsOnChannelSwitch(t *testing.T) {
	const rate = 64.0
	tr := NewTracker(rate, 2)

	strong, ts := pulseTrain(int(rate*30), rate, 72, 100000, 0.04)
	weak, _ := pulseTrain(int(rate*30), rate, 72, 50000, 0.005)
	feed(tr, [][]float64{strong, weak}, ts, 16)
	if got := tr.Snapshot().Channel; got != 0 {
		t.Fatalf("Channel = %d, want 0 initially", got)
	}

	// Channel 1 now modulates far more and clears ChallengerMargin.
	strong2, ts2 := pulseTrain(int(rate*30), rate, 72, 100000, 0.005)
	weak2, _ := pulseTrain(int(rate*30), rate, 72, 50000, 0.06)
	for i := range ts2 {
		ts2[i] = ts[len(ts)-1] + float64(i+1)/rate
	}
	feed(tr, [][]float64{strong2, weak2}, ts2, 16)

	if got := tr.Snapshot().Channel; got != 1 {
		t.Errorf("Channel = %d, want 1 after the switch", got)
	}
}
