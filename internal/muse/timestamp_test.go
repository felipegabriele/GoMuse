package muse

import (
	"math"
	"math/rand"
	"testing"
)

const (
	testRate = 256.0
	testSPP  = 12
)

func assertMonotonic(t *testing.T, name string, ts []float64, prev float64) float64 {
	t.Helper()
	for i, v := range ts {
		if v <= prev {
			t.Fatalf("%s: timestamp %d (%v) not greater than previous (%v)", name, i, v, prev)
		}
		prev = v
	}
	return prev
}

// TestDejitterRegularSeries feeds arrivals that lie exactly on the nominal
// 1/256 s line, so the RLS fit has nothing to correct.
func TestDejitterRegularSeries(t *testing.T) {
	const t0 = 1000.0
	d := NewDejitter(testRate, testSPP, t0)

	prev := math.Inf(-1)
	for k := 0; k < 300; k++ {
		// Arrival of the chunk whose last sample has index 12k+11.
		arrival := t0 + float64(testSPP*k+testSPP-1)/testRate
		ts := d.Next(uint16(k), arrival)
		if len(ts) != testSPP {
			t.Fatalf("packet %d: got %d timestamps, want %d", k, len(ts), testSPP)
		}
		prev = assertMonotonic(t, "regular", ts, prev)
	}
	if math.Abs(d.Slope()-1/testRate) > 1e-9 {
		t.Errorf("slope = %v, want %v", d.Slope(), 1/testRate)
	}
}

// TestDejitterConvergesFromWrongNominalRate starts the filter at 250 Hz while
// the device really runs at 256 Hz; the fit must pull the slope over.
func TestDejitterConvergesFromWrongNominalRate(t *testing.T) {
	const t0 = 0.0
	d := NewDejitter(250, testSPP, t0)

	for k := 0; k < 500; k++ {
		arrival := t0 + float64(testSPP*k+testSPP-1)/testRate
		d.Next(uint16(k), arrival)
	}
	want := 1 / testRate
	if math.Abs(d.Slope()-want)/want > 0.001 {
		t.Errorf("slope = %v, want within 0.1%% of %v", d.Slope(), want)
	}
}

// TestDejitterJitteredSeries adds bounded noise to the arrival times, which is
// what BLE notification batching actually looks like.
func TestDejitterJitteredSeries(t *testing.T) {
	const t0 = 500.0
	rng := rand.New(rand.NewSource(20260726))
	d := NewDejitter(testRate, testSPP, t0)

	prev := math.Inf(-1)
	for k := 0; k < 600; k++ {
		// +/- 10 ms of jitter, comparable to a BLE connection interval.
		jitter := (rng.Float64() - 0.5) * 0.02
		arrival := t0 + float64(testSPP*k+testSPP-1)/testRate + jitter
		ts := d.Next(uint16(k), arrival)
		prev = assertMonotonic(t, "jittered", ts, prev)
	}
	want := 1 / testRate
	if math.Abs(d.Slope()-want)/want > 0.02 {
		t.Errorf("slope = %v, want within 2%% of %v", d.Slope(), want)
	}
}

// TestDejitterCounterWrap checks that 65535 -> 0 is treated as a normal
// increment, not as 65535 lost packets.
func TestDejitterCounterWrap(t *testing.T) {
	const t0 = 10.0
	d := NewDejitter(testRate, testSPP, t0)

	arrival := t0
	step := float64(testSPP) / testRate
	for _, tm := range []uint16{65533, 65534, 65535, 0, 1} {
		d.Next(tm, arrival)
		arrival += step
	}
	// Five packets, each advancing the index by exactly one chunk.
	if got := d.SampleIndex(); got != int64(5*testSPP) {
		t.Errorf("sample index = %d, want %d", got, 5*testSPP)
	}
}

// TestDejitterGapAdvancesIndex checks that a real hole in the counter moves the
// time base forward.
func TestDejitterGapAdvancesIndex(t *testing.T) {
	const t0 = 10.0
	d := NewDejitter(testRate, testSPP, t0)

	d.Next(10, t0)
	if got := d.SampleIndex(); got != int64(testSPP) {
		t.Fatalf("after first packet index = %d, want %d", got, testSPP)
	}
	d.Next(15, t0+float64(testSPP)/testRate)

	// The counter stepped 10 -> 15, so packets 11..14 were lost: four of them.
	// Advance over those four, then over the chunk just delivered.
	// (muse-lsl uses tm-lastTm+1 = 6 here, which over-counts by two packets.)
	want := int64(testSPP + testSPP*4 + testSPP)
	if got := d.SampleIndex(); got != want {
		t.Errorf("sample index = %d, want %d", got, want)
	}
}

// TestDejitterWrapWithLoss covers the case muse-lsl's -65535 special case
// misses: a counter wrap that also drops packets. 65534 -> 1 is three steps,
// so two packets were lost.
func TestDejitterWrapWithLoss(t *testing.T) {
	d := NewDejitter(testRate, testSPP, 0)
	d.Next(65534, 0)
	d.Next(1, float64(3*testSPP)/testRate)

	want := int64(testSPP + testSPP*2 + testSPP)
	if got := d.SampleIndex(); got != want {
		t.Errorf("sample index = %d, want %d", got, want)
	}
}

// TestDejitterDuplicateDoesNotRewind: a repeated or out-of-order counter must
// never move the sample index backwards, or timestamps stop being monotonic.
func TestDejitterDuplicateDoesNotRewind(t *testing.T) {
	d := NewDejitter(testRate, testSPP, 0)
	d.Next(100, 0)
	before := d.SampleIndex()
	d.Next(100, 0.05) // duplicate
	d.Next(98, 0.10)  // arrived late, out of order
	if got := d.SampleIndex(); got < before {
		t.Errorf("sample index went backwards: %d < %d", got, before)
	}
}

// TestDejitterNoGapOnConsecutive guards against the gap branch firing on the
// ordinary +1 case.
func TestDejitterNoGapOnConsecutive(t *testing.T) {
	d := NewDejitter(testRate, testSPP, 0)
	for k := 0; k < 10; k++ {
		d.Next(uint16(100+k), float64(k)*float64(testSPP)/testRate)
	}
	if got := d.SampleIndex(); got != int64(10*testSPP) {
		t.Errorf("sample index = %d, want %d", got, 10*testSPP)
	}
}

// TestDejitterFirstPacketNoGap: the first packet has no predecessor, so its
// arbitrary counter value must not be read as a gap.
func TestDejitterFirstPacketNoGap(t *testing.T) {
	d := NewDejitter(testRate, testSPP, 0)
	d.Next(31337, 0)
	if got := d.SampleIndex(); got != int64(testSPP) {
		t.Errorf("sample index = %d, want %d", got, testSPP)
	}
}

// TestDejitterTimestampSpacing: within a chunk, samples are one slope apart and
// the last one carries the index used for the fit.
func TestDejitterTimestampSpacing(t *testing.T) {
	const t0 = 100.0
	d := NewDejitter(testRate, 6, t0)
	ts := d.Next(0, t0)
	if len(ts) != 6 {
		t.Fatalf("got %d timestamps, want 6", len(ts))
	}
	for i := 1; i < len(ts); i++ {
		if math.Abs((ts[i]-ts[i-1])-d.Slope()) > 1e-12 {
			t.Errorf("gap %d = %v, want slope %v", i, ts[i]-ts[i-1], d.Slope())
		}
	}
	if math.Abs(ts[0]-(d.Slope()*0+d.Offset())) > 1e-12 {
		t.Errorf("first timestamp = %v, want %v", ts[0], d.Offset())
	}
}
