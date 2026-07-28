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

// feedWithDrops pushes one sample per call, as an Athena does — optics tag 0x36
// carries a single sample per packet (internal/muse/athena.go) — discarding
// every dropEvery'th sample so its timestamp is simply missing. That is exactly
// what a hub drop looks like downstream: internal/hub drops newest on a full
// buffer and CLAUDE.md notes a browser subscriber drops routinely by design.
// dropEvery of 0 drops nothing.
func feedWithDrops(tr *Tracker, ch, ts []float64, dropEvery int) {
	for i := range ts {
		if dropEvery > 0 && i%dropEvery == dropEvery-1 {
			continue
		}
		tr.Process([][]float64{ch[i : i+1]}, ts[i:i+1])
	}
}

// Routine sample loss must not stop the feature from locking. Holes of a single
// sample are the common case on the target device, and treating each one as lost
// data — breaking the interval chain and wiping the filter and peak state — used
// to leave the tracker permanently in acquiring at around one drop per second,
// because every chain segment was then shorter than one beat period.
func TestTrackerLocksDespiteDroppedSamples(t *testing.T) {
	const rate, bpm = 64.0, 72.0

	for _, dropEvery := range []int{0, 640, 128, 64, 32} {
		pulseCh, ts := pulseTrain(int(rate*60), rate, bpm, 100000, 0.04)
		tr := NewTracker(rate, 1)
		feedWithDrops(tr, pulseCh, ts, dropEvery)

		snap := tr.Snapshot()
		t.Logf("1 drop per %-4d: state=%-9s bpm=%.1f hasBPM=%v intervals=%d",
			dropEvery, snap.State, snap.BPM, snap.HasBPM, tr.hrv.IntervalCount())

		if snap.State != StateLocked {
			t.Errorf("drop 1 per %d: State = %q, want %q", dropEvery, snap.State, StateLocked)
			continue
		}
		if math.Abs(snap.BPM-bpm) > 3 {
			t.Errorf("drop 1 per %d: BPM = %.1f, want %.0f +/- 3", dropEvery, snap.BPM, bpm)
		}
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
	first, ts := pulseTrain(int(rate*20), rate, 72, 100000, 0.04)

	tr := NewTracker(rate, 1)
	feed(tr, [][]float64{first}, ts, 16)
	before := tr.hrv.IntervalCount()
	if before < 5 {
		t.Fatalf("only %d intervals after 20 s, want >=5", before)
	}

	// A second run starting 1 s later: samples were lost in between. The gap is
	// short enough that the beat closing it lands within MinIntervalMs/
	// MaxIntervalMs of the last beat before the gap — AddBeat's own range check
	// would accept it. Break is therefore the only thing that can suppress it,
	// which is what this test needs to actually exercise.
	second, sts := pulseTrain(int(rate*20), rate, 72, 100000, 0.04)
	for i := range sts {
		sts[i] = ts[len(ts)-1] + 1.0 + float64(i)/rate
	}
	feed(tr, [][]float64{second}, sts, 16)

	// Total span is under BeatHistorySec, so nothing is trimmed and
	// Snapshot().BeatCount equals the HRV's own beat count exactly: the
	// windowing-basis mismatch that affects longer runs does not apply here.
	snap := tr.Snapshot()
	if got := tr.hrv.IntervalCount(); got != snap.BeatCount-2 {
		t.Errorf("IntervalCount = %d with %d beats, want %d: an interval spans the gap",
			got, snap.BeatCount, snap.BeatCount-2)
	}
}

// Taking the headband off and putting it back on must go through acquiring.
// Retaining the pre-loss intervals let BPM's 7-interval median keep reporting the
// old rate as locked for several beats after the pulse returned.
func TestTrackerReacquiresAfterLosingThePulse(t *testing.T) {
	const rate = 64.0

	beat, ts := pulseTrain(int(rate*30), rate, 72, 100000, 0.04)
	tr := NewTracker(rate, 1)
	feed(tr, [][]float64{beat}, ts, 16)
	snap := tr.Snapshot()
	if snap.State != StateLocked || !snap.HasBPM {
		t.Fatalf("State = %q HasBPM = %v after 30 s of pulse, want %q with a rate",
			snap.State, snap.HasBPM, StateLocked)
	}
	stale := snap.BPM

	// Headband off: the channel stops modulating and the selector drops it.
	flat, fts := pulseTrain(int(rate*8), rate, 72, 100000, 0.0002)
	for i := range fts {
		fts[i] = ts[len(ts)-1] + float64(i+1)/rate
	}
	feed(tr, [][]float64{flat}, fts, 16)
	snap = tr.Snapshot()
	if snap.State != StateNoPulse || snap.Channel != -1 {
		t.Fatalf("State = %q Channel = %d after 8 s flat, want %q and -1",
			snap.State, snap.Channel, StateNoPulse)
	}
	if got := tr.hrv.IntervalCount(); got != 0 {
		t.Errorf("IntervalCount = %d after losing the pulse, want 0: intervals "+
			"measured before the loss were retained", got)
	}

	// Back on, and at a distinctly different rate so a stale readout is
	// unmistakable.
	slow, sts := pulseTrain(int(rate*5), rate, 40, 100000, 0.04)
	for i := range sts {
		sts[i] = fts[len(fts)-1] + float64(i+1)/rate
	}
	for i := 0; i < len(sts); i += 16 {
		end := i + 16
		if end > len(sts) {
			end = len(sts)
		}
		tr.Process([][]float64{slow[i:end]}, sts[i:end])
		if s := tr.Snapshot(); s.HasBPM && math.Abs(s.BPM-stale) < 1 {
			t.Fatalf("reported the pre-loss rate %.1f bpm as %q after regaining "+
				"the pulse at 40 bpm", s.BPM, s.State)
		}
	}
	// Five seconds at 40 bpm is at most three beats, so fewer than
	// LockIntervals intervals can have formed.
	if got := tr.Snapshot().State; got != StateAcquiring {
		t.Errorf("State = %q 5 s after regaining the pulse, want %q", got, StateAcquiring)
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

	// Feed one sample at a time so the exact call that flips the selected
	// channel can be caught, and check its aftermath directly: that call is
	// the only place Reset and Break are supposed to run.
	switched := false
	for i := range ts2 {
		before := tr.channel
		tr.Process([][]float64{strong2[i : i+1], weak2[i : i+1]}, ts2[i:i+1])
		if tr.channel == before {
			continue
		}
		switched = true

		// hrv.Break clears haveLast; a single fresh sample cannot itself
		// complete a beat, so if Break ran this must still be false. Without
		// it, the last beat from channel 0 would still be pending and able to
		// pair with channel 1's first beat into a bogus cross-channel interval.
		if tr.hrv.haveLast {
			t.Error("hrv.haveLast = true right after the channel switch, want false (Break did not run)")
		}

		// det.Reset should leave the detector as if this were the first sample
		// it had ever seen: compare against a brand-new detector fed the same
		// sample. Without Reset, stale filter/peak history from channel 0 (a
		// different pedestal, unrelated phase) would still be mixed in.
		want := NewDetector(rate)
		want.Process(weak2[i:i+1], ts2[i:i+1])
		if tr.det.have != want.have || tr.det.peak != want.peak {
			t.Errorf("detector state after switch = {have:%d peak:%g}, want {have:%d peak:%g} (Reset did not run)",
				tr.det.have, tr.det.peak, want.have, want.peak)
		}
		break
	}
	if !switched {
		t.Fatal("channel never switched to 1")
	}
	if got := tr.Snapshot().Channel; got != 1 {
		t.Errorf("Channel = %d, want 1 after the switch", got)
	}
}
