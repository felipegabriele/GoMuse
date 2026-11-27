package muse

import (
	"math"
	"sync"
	"time"
)

// ClockHost names where every timestamp comes from: the wall clock of the
// computer running gomuse. The headband's own clock is never read.
const ClockHost = "host-wall-clock"

// StreamTiming says how one stream's timestamps were produced, so a recording
// can be interpreted later without reading the source code.
type StreamTiming struct {
	Stream string `json:"stream"`
	// Method is "rls-fit" when the timestamps lie on a line fitted to arrival
	// times (EEG, PPG, and everything on an Athena), or "arrival" when they are
	// arrival times spread back over the nominal sample period (ACC, gyro on a
	// classic headband).
	Method string `json:"method"`
	Clock  string `json:"clock"`
	// NominalRateHz is the rate the headband is specified to run at.
	NominalRateHz float64 `json:"nominal_rate_hz"`
	// AnchorUnixS is the timestamp assigned to the stream's first sample. For an
	// rls-fit stream it is the arrival of the first complete packet, so it
	// includes the Bluetooth and operating-system delay of that packet.
	AnchorUnixS float64 `json:"anchor_unix_s"`
	// EstimatedPeriodS and EstimatedRateHz are the fit's current estimate of the
	// sample period, which differs from nominal because the headband's clock is
	// not the computer's. Zero for arrival-method streams.
	EstimatedPeriodS float64 `json:"estimated_period_s,omitempty"`
	EstimatedRateHz  float64 `json:"estimated_rate_hz,omitempty"`
	Samples          int64   `json:"samples"`
}

// ClockJump is a step of the computer's wall clock seen during the run, as
// opposed to the gradual drift an NTP client corrects by slewing.
type ClockJump struct {
	AtUnixS float64 `json:"at_unix_s"`
	// Ms is how far the wall clock moved relative to the monotonic clock,
	// positive when it jumped forward.
	Ms float64 `json:"ms"`
}

// Timing is the timing report for a whole run.
type Timing struct {
	Streams    []StreamTiming `json:"streams"`
	ClockJumps []ClockJump    `json:"clock_jumps"`
}

// timing returns the fit-based report for a Dejitter. Callers hold whatever
// lock guards d.
func (d *Dejitter) timing(name string, nominalRate float64) StreamTiming {
	t := StreamTiming{
		Stream:        name,
		Method:        "rls-fit",
		Clock:         ClockHost,
		NominalRateHz: nominalRate,
		AnchorUnixS:   d.offset,
		Samples:       d.sampleIndex,
	}
	if d.slope > 0 {
		t.EstimatedPeriodS = d.slope
		t.EstimatedRateHz = 1 / d.slope
	}
	return t
}

// arrivalStat accumulates what is known about a stream that is stamped
// straight from arrival time.
type arrivalStat struct {
	mu      sync.Mutex
	first   float64
	samples int64
}

func (a *arrivalStat) add(firstTimestamp float64, n int) {
	a.mu.Lock()
	if a.samples == 0 {
		a.first = firstTimestamp
	}
	a.samples += int64(n)
	a.mu.Unlock()
}

func (a *arrivalStat) timing(name string, nominalRate float64) StreamTiming {
	a.mu.Lock()
	defer a.mu.Unlock()
	return StreamTiming{
		Stream:        name,
		Method:        "arrival",
		Clock:         ClockHost,
		NominalRateHz: nominalRate,
		AnchorUnixS:   a.first,
		Samples:       a.samples,
	}
}

// clockJumpThreshold is the smallest step of the wall clock worth reporting.
// Slewing by an NTP client changes the clock by far less than this per second.
const clockJumpThreshold = 50 * time.Millisecond

// clockWatch detects steps of the wall clock. Go's time.Now carries both a wall
// and a monotonic reading; their difference stays constant unless something
// sets the system clock.
type clockWatch struct {
	mu        sync.Mutex
	startWall time.Time
	lastSkew  time.Duration
	jumps     []ClockJump
	onJump    func(ClockJump)
}

func newClockWatch(start time.Time, onJump func(ClockJump)) *clockWatch {
	return &clockWatch{startWall: start.Round(0), onJump: onJump}
}

// check compares the wall time with the monotonic time elapsed since the watch
// began. It is separate from tick so tests can supply both.
func (w *clockWatch) check(wallNow time.Time, monoElapsed time.Duration) {
	skew := wallNow.Round(0).Sub(w.startWall) - monoElapsed
	w.mu.Lock()
	step := skew - w.lastSkew
	w.lastSkew = skew
	var j ClockJump
	hit := time.Duration(math.Abs(float64(step))) >= clockJumpThreshold
	if hit {
		j = ClockJump{
			AtUnixS: float64(wallNow.UnixNano()) / 1e9,
			Ms:      float64(step) / float64(time.Millisecond),
		}
		w.jumps = append(w.jumps, j)
	}
	cb := w.onJump
	w.mu.Unlock()
	if hit && cb != nil {
		cb(j)
	}
}

// tick reads the clocks now. It needs the original start with its monotonic
// reading, which is why it takes it from the caller rather than the stored,
// stripped copy.
func (w *clockWatch) tick(start time.Time) {
	now := time.Now()
	w.check(now, now.Sub(start))
}

func (w *clockWatch) list() []ClockJump {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]ClockJump, len(w.jumps))
	copy(out, w.jumps)
	return out
}
