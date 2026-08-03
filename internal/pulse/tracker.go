package pulse

// State is what the tracker can honestly say about the pulse right now.
//
// Three states exist rather than two so the viewer never shows a fabricated
// number: "no pulse" and "acquiring" look identical to a user reading bpm, but
// they mean different things and only one of them will resolve by waiting.
type State string

const (
	// StateNoPulse means no channel carries enough modulation — the headband
	// is off the head, or the preset sends nothing usable.
	StateNoPulse State = "no pulse"
	// StateAcquiring means a channel is selected but too few clean intervals
	// have accumulated to report a rate.
	StateAcquiring State = "acquiring"
	// StateLocked means the numbers are real.
	StateLocked State = "locked"
)

// Snapshot is the tracker's state at one instant, safe to serialise.
//
// The Has* flags exist rather than sentinel values because "no number yet" and
// "a rate of zero" must not be confusable.
type Snapshot struct {
	State     State
	Channel   int // -1 when none is selected
	BPM       float64
	HasBPM    bool
	RMSSD     float64
	HasRMSSD  bool
	IBIMs     float64
	HasIBI    bool
	BeatCount int
	Beats     []float64
}

// Tracker wires Selector, Detector and HRV into one unit: it takes raw
// channel-major pulse samples and answers with a rate and an HRV figure.
type Tracker struct {
	sel *Selector
	det *Detector
	hrv *HRV

	channel int
	lastTS  float64
}

// NewTracker returns a tracker for a pulse stream of the given rate and width.
func NewTracker(rate float64, channels int) *Tracker {
	return &Tracker{
		sel:     NewSelector(rate, channels),
		det:     NewDetector(rate),
		hrv:     NewHRV(),
		channel: -1,
	}
}

// Process feeds one chunk of the pulse stream. data is channel-major and is
// neither retained nor modified.
func (t *Tracker) Process(data [][]float64, timestamps []float64) {
	if len(data) == 0 || len(timestamps) == 0 {
		return
	}
	t.lastTS = timestamps[len(timestamps)-1]

	t.sel.Observe(data)
	ch := t.sel.Channel()

	if ch != t.channel {
		// The new channel has a different pedestal and unrelated phase, so the
		// filter history describes a signal that is no longer being watched.
		t.det.Reset()
		// A full Reset, not merely Break: every accumulated interval was
		// measured on a channel that is no longer selected, and this branch also
		// covers losing the pulse entirely (ch going to -1) and regaining it.
		// Keeping them would let BPM's 7-interval median report the pre-loss
		// rate as locked for several beats after the headband is put back on,
		// skipping acquiring — which defeats the point of having three states.
		t.hrv.Reset()
		t.channel = ch
	}
	if ch < 0 || ch >= len(data) {
		return
	}

	beats, gap := t.det.Process(data[ch], timestamps)
	if gap {
		t.hrv.Break()
	}
	for _, b := range beats {
		t.hrv.AddBeat(b)
	}
}

// Snapshot reports the current state. Beats covers the last BeatHistorySec.
func (t *Tracker) Snapshot() Snapshot {
	s := Snapshot{Channel: t.channel, State: StateNoPulse}
	if t.channel < 0 {
		return s
	}

	s.State = StateAcquiring
	s.Beats = t.hrv.Beats(t.lastTS - BeatHistorySec)
	s.BeatCount = len(s.Beats)

	bpm, ok := t.hrv.BPM()
	if !ok {
		return s
	}
	s.State, s.BPM, s.HasBPM = StateLocked, bpm, true
	s.RMSSD, s.HasRMSSD = t.hrv.RMSSD()
	s.IBIMs, s.HasIBI = t.hrv.LastIntervalMs()
	return s
}
