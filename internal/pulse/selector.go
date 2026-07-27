// Package pulse turns a headband's raw pulse stream into a heart rate and an
// HRV figure.
//
// It is deliberately independent of package sample: it takes channel-major
// [][]float64 the way internal/dsp does, so it can be unit-tested on synthetic
// waveforms with no notion of a device or a stream.
//
// The chain is Selector -> Detector -> HRV, assembled by Tracker.
package pulse

import "sort"

const (
	// WindowSec is how much history the channel ranking looks at, and EvalSec
	// how often it re-ranks. Four seconds spans three beats at a resting rate,
	// enough for the percentile spread to reflect the pulse rather than a
	// single beat's shape.
	WindowSec = 4.0
	EvalSec   = 2.0

	// ModulationFloor is the smallest (p95-p5)/mean that counts as a pulse.
	// Measured on a MuseS-1FF3 at preset p1041: OPT12/13 reach ~0.04 and
	// OPT8/9 ~0.02, while OPT4-7 sit at ~0.0006. This floor admits the former
	// and excludes the latter.
	ModulationFloor = 0.001

	// ChallengerMargin is how much better a channel must be to unseat the
	// incumbent. Without it, two near-equal channels swap every evaluation and
	// the displayed channel name flickers.
	ChallengerMargin = 1.20

	// MinMean is the absolute floor below which a channel is treated as
	// carrying nothing. decodeAthenaOptics zero-fills the canonical channels a
	// tag does not carry (tag 0x34 fills only slots 4-7, 0x35 only 0-7), so on
	// presets other than p1041 some channels are identically zero and their
	// mean is exactly 0. Dividing by that is 0/0. Go yields NaN, which happens
	// to lose every > comparison and so would never be selected, but relying on
	// that is accidental. One raw count is far below any real optical reading.
	MinMean = 1.0
)

// Selector ranks pulse channels by how strongly they modulate and picks one.
//
// Which channel carries the best pulse depends on headband fit and on the
// preset, so a fixed choice is wrong: on the reference capture the best channel
// modulates sixty times more than the worst.
type Selector struct {
	window int // samples retained per channel
	every  int // samples between evaluations

	buf   [][]float64 // rolling history, channel-major
	since int         // samples seen since the last evaluation

	chosen int
	score  float64
}

// NewSelector returns a selector for a stream of the given rate and width.
func NewSelector(rate float64, channels int) *Selector {
	if rate <= 0 {
		panic("pulse: rate must be positive")
	}
	if channels <= 0 {
		panic("pulse: channels must be positive")
	}
	s := &Selector{
		window: int(rate * WindowSec),
		every:  int(rate * EvalSec),
		buf:    make([][]float64, channels),
		chosen: -1,
	}
	if s.window < 1 {
		s.window = 1
	}
	if s.every < 1 {
		s.every = 1
	}
	return s
}

// Observe appends a channel-major block and re-ranks when enough new samples
// have arrived. It does not retain or modify data.
func (s *Selector) Observe(data [][]float64) {
	if len(data) != len(s.buf) || len(data) == 0 {
		return
	}
	n := len(data[0])
	if n == 0 {
		return
	}

	for c := range s.buf {
		s.buf[c] = append(s.buf[c], data[c]...)
		if excess := len(s.buf[c]) - s.window; excess > 0 {
			s.buf[c] = append(s.buf[c][:0], s.buf[c][excess:]...)
		}
	}

	s.since += n
	if s.since >= s.every && len(s.buf[0]) >= s.window {
		s.since = 0
		s.evaluate()
	}
}

// Channel returns the selected channel index, or -1 when none qualifies.
func (s *Selector) Channel() int { return s.chosen }

func (s *Selector) evaluate() {
	best, bestScore := -1, 0.0
	for c := range s.buf {
		score := modulation(s.buf[c])
		if score > bestScore {
			best, bestScore = c, score
		}
	}

	if best < 0 || bestScore < ModulationFloor {
		s.chosen, s.score = -1, 0
		return
	}
	// Hold the incumbent unless the challenger clears the margin.
	if s.chosen >= 0 && best != s.chosen && bestScore < s.score*ChallengerMargin {
		// Keep the incumbent, but refresh its score so a decaying incumbent
		// can eventually be beaten.
		s.score = modulation(s.buf[s.chosen])
		if s.score >= ModulationFloor {
			return
		}
	}
	s.chosen, s.score = best, bestScore
}

// modulation is (p95-p5)/mean, a scale-free measure of how much a channel
// swings relative to its own pedestal. Returns 0 for a channel carrying
// nothing, so it can never be selected.
func modulation(x []float64) float64 {
	if len(x) < 2 {
		return 0
	}
	sum := 0.0
	for _, v := range x {
		sum += v
	}
	mean := sum / float64(len(x))
	if mean <= MinMean {
		return 0
	}

	sorted := make([]float64, len(x))
	copy(sorted, x)
	sort.Float64s(sorted)
	return (percentile(sorted, 0.95) - percentile(sorted, 0.05)) / mean
}

// percentile indexes into an already-sorted slice by nearest rank.
func percentile(sorted []float64, p float64) float64 {
	i := int(p * float64(len(sorted)-1))
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
