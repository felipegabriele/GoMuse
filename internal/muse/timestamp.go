package muse

// Dejitter turns jittery BLE notification arrival times into a smooth,
// monotonic time base, mirroring muse-lsl's _init_timestamp_correction /
// _update_timestamp_correction.
//
// BLE delivers notifications in bursts: several packets land in the same
// connection event and their arrival times cluster, even though the underlying
// samples are evenly spaced. Stamping samples with arrival times would produce
// a stream that stutters and occasionally goes backwards. Instead we treat the
// sample index as the independent variable and fit
//
//	arrival ≈ offset + slope*index
//
// by recursive least squares, then emit timestamps from the fitted line. Only
// the slope (the true sample period, which drifts from nominal because the
// headband's clock is not ours) is estimated; the offset is fixed at the time
// the stream started, exactly as in muse-lsl.
type Dejitter struct {
	samplesPerPacket int

	// offset and slope are muse-lsl's reg_params: [intercept, period].
	offset float64
	slope  float64

	// p is the scalar RLS covariance. Seeding it small (1e-4) says "the
	// nominal rate is already close", so early noisy packets cannot yank the
	// estimate far off.
	p float64

	sampleIndex int64

	lastTm  int
	started bool
}

// NewDejitter creates a Dejitter for a stream of the given nominal rate (Hz)
// and samples per packet. now is the wall-clock time, in seconds, that anchors
// the emitted timestamps; it is a parameter rather than a call to time.Now so
// that callers (and tests) control the clock.
func NewDejitter(rate float64, samplesPerPacket int, now float64) *Dejitter {
	return &Dejitter{
		samplesPerPacket: samplesPerPacket,
		offset:           now,
		slope:            1 / rate,
		p:                1e-4,
	}
}

// Slope returns the current estimate of the seconds between samples.
func (d *Dejitter) Slope() float64 { return d.slope }

// Offset returns the fixed intercept: the timestamp assigned to sample 0.
func (d *Dejitter) Offset() float64 { return d.offset }

// SampleIndex returns the index the next chunk will start at.
func (d *Dejitter) SampleIndex() int64 { return d.sampleIndex }

// Next records a packet and returns timestamps for its samples.
//
// packetIndex is the headband's uint16 counter; arrival is the wall-clock time
// in seconds at which the packet was received. For a multi-characteristic
// stream such as EEG, pass the minimum arrival across the channels of the
// chunk — that is the least delayed, and so the least biased, observation.
func (d *Dejitter) Next(packetIndex uint16, arrival float64) []float64 {
	tm := int(packetIndex)
	if d.started {
		d.handleGap(tm)
	}
	d.lastTm = tm
	d.started = true

	start := d.sampleIndex
	d.sampleIndex += int64(d.samplesPerPacket)

	// The fit is anchored on the last sample of the chunk, since arrival is
	// observed once the whole chunk has been received.
	d.update(float64(start+int64(d.samplesPerPacket)-1), arrival)

	ts := make([]float64, d.samplesPerPacket)
	for i := range ts {
		ts[i] = d.slope*float64(start+int64(i)) + d.offset
	}
	return ts
}

// NextArrival records a packet of n samples received at the given wall-clock
// time (in seconds) and returns dejittered timestamps for them.
//
// It is the entry point for the Athena protocol, which has no per-stream packet
// counter to key off: several streams are multiplexed into one notification
// stream, so a lost packet shows up only as a larger arrival interval and there
// is no equivalent of Next's gap handling. muse-lsl's athena.py behaves the
// same way — its RLSTimestampCorrector consumes n samples per call and never
// inspects an index — so a drop shifts the fit rather than being counted.
//
// The samplesPerPacket given to NewDejitter is unused here, since Athena packet
// sizes vary by sensor tag (an EEG packet carries 4 samples on tag 0x11 and 2
// on tag 0x12).
func (d *Dejitter) NextArrival(n int, arrival float64) []float64 {
	if n <= 0 {
		return nil
	}
	start := d.sampleIndex
	d.sampleIndex += int64(n)

	// As in Next, the fit is anchored on the chunk's last sample: arrival is
	// observed once the whole packet has been received.
	d.update(float64(start+int64(n)-1), arrival)

	ts := make([]float64, n)
	for i := range ts {
		ts[i] = d.slope*float64(start+int64(i)) + d.offset
	}
	return ts
}

// maxPlausibleGap bounds how many missing packets we will believe in. Beyond
// half the counter range a positive gap is indistinguishable from a packet
// arriving out of order, so we treat it as reordering and leave the index
// alone rather than jumping the time base by minutes.
const maxPlausibleGap = 1 << 15

// handleGap advances the sample index over packets that never arrived, so that
// the time base stays tied to real elapsed time instead of compressing the gap
// away.
//
// Deliberate divergence from muse-lsl, which advances by
// samplesPerPacket*(tm-lastTm+1). If the counter steps by N then N-1 packets
// were lost, so that expression over-counts by two packets on every drop —
// about 94 ms at 256 Hz, and it accumulates. We advance by the N-1 that were
// actually missed.
//
// The wrap is handled by doing the subtraction in uint16, so 65535 -> 0 is a
// step of 1 and 65534 -> 1 is a step of 3, without needing muse-lsl's special
// case for exactly -65535 (which only covered a wrap that lost nothing).
func (d *Dejitter) handleGap(tm int) {
	elapsed := uint16(tm) - uint16(d.lastTm)
	if elapsed <= 1 || elapsed > maxPlausibleGap {
		// 1 is the ordinary case; 0 is a duplicate and anything above the
		// bound is reordering. None of them should move the index, and a
		// negative move would break monotonicity.
		return
	}
	d.sampleIndex += int64(d.samplesPerPacket) * int64(elapsed-1)
}

// update is one step of scalar recursive least squares on the slope, with the
// intercept held fixed. tSource is the sample index of the observation,
// tReceiver its arrival time.
func (d *Dejitter) update(tSource, tReceiver float64) {
	// Work relative to the fixed intercept; the regression has one free
	// parameter, so the residual is measured from the anchor time.
	y := tReceiver - d.offset
	t2 := tSource * tSource

	// Covariance update. As tSource grows the denominator approaches zero from
	// above and p shrinks towards it, which is what makes the filter settle:
	// later packets move the estimate less and less.
	d.p = d.p - (d.p*d.p*t2)/(1-d.p*t2)
	// Gain times innovation: how far this arrival fell from the current line.
	d.slope = d.slope + d.p*tSource*(y-tSource*d.slope)
}
