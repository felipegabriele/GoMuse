package pulse

import "testing"

// ramp builds a channel of n samples: a constant pedestal plus a triangle wave
// of the given fractional modulation. A triangle is used rather than a sine
// because its p5 and p95 are easy to reason about.
func ramp(n int, pedestal, modulation float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		phase := float64(i%20) / 20.0 // 20-sample period
		if phase > 0.5 {
			phase = 1 - phase
		}
		out[i] = pedestal * (1 + modulation*(phase*4-1))
	}
	return out
}

func TestSelectorPicksStrongestChannel(t *testing.T) {
	s := NewSelector(64, 3)
	// Channel 1 modulates 4%, the others 0.05% — below ModulationFloor.
	data := [][]float64{
		ramp(256, 100000, 0.0005),
		ramp(256, 100000, 0.04),
		ramp(256, 100000, 0.0005),
	}
	s.Observe(data)

	if got := s.Channel(); got != 1 {
		t.Errorf("Channel() = %d, want 1 (the only channel above the floor)", got)
	}
}

func TestSelectorReportsNoneWhenAllFlat(t *testing.T) {
	s := NewSelector(64, 3)
	data := [][]float64{
		ramp(256, 100000, 0.0001),
		ramp(256, 100000, 0.0001),
		ramp(256, 100000, 0.0001),
	}
	s.Observe(data)

	if got := s.Channel(); got != -1 {
		t.Errorf("Channel() = %d, want -1 when nothing clears the floor", got)
	}
}

// An Athena tag that does not carry a canonical channel zero-fills it. Dividing
// by that channel's mean is 0/0; the guard must be explicit rather than relying
// on NaN losing comparisons by accident.
func TestSelectorSkipsZeroFilledChannels(t *testing.T) {
	s := NewSelector(64, 4)
	zeros := make([]float64, 256)
	data := [][]float64{zeros, zeros, ramp(256, 100000, 0.04), zeros}
	s.Observe(data)

	if got := s.Channel(); got != 2 {
		t.Errorf("Channel() = %d, want 2 (the only carrying channel)", got)
	}
}

func TestSelectorHysteresisPreventsFlapping(t *testing.T) {
	s := NewSelector(64, 2)
	// Channel 0 wins the first evaluation.
	s.Observe([][]float64{ramp(256, 100000, 0.040), ramp(256, 100000, 0.030)})
	if got := s.Channel(); got != 0 {
		t.Fatalf("Channel() = %d, want 0 initially", got)
	}
	// Channel 1 now leads, but by only 10% — under ChallengerMargin.
	s.Observe([][]float64{ramp(256, 100000, 0.040), ramp(256, 100000, 0.044)})
	if got := s.Channel(); got != 0 {
		t.Errorf("Channel() = %d, want 0 held: a 10%% lead must not unseat it", got)
	}
	// A 50% lead does clear the margin.
	s.Observe([][]float64{ramp(256, 100000, 0.040), ramp(256, 100000, 0.060)})
	if got := s.Channel(); got != 1 {
		t.Errorf("Channel() = %d, want 1 once the challenger clears the margin", got)
	}
}
