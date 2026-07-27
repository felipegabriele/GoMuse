// Package synth generates synthetic Muse-like sensor data and publishes it to
// a hub at each stream's real rate and packet size.
//
// It exists so that the whole path from hub to browser — encoding, filtering,
// fan-out, the viewer itself — can be exercised, tested and demoed without a
// headband. The waveforms are not a physiological model; they are chosen to
// make the viewer's features observable:
//
//   - EEG carries a 10 Hz alpha rhythm on 1/f-ish noise, so a spectrum or a
//     scrolling trace looks like EEG rather than like static, plus a large,
//     slow DC drift on two channels. The drift is the point: it is what the
//     filter toggle visibly removes.
//   - PPG is a pulse-shaped waveform on a huge DC pedestal, matching the real
//     device's ~1e5..2e6 raw counts, so axis autoscaling is exercised.
//   - ACC rests near (0, 0, 1) g with a slow sway; Gyro sits near zero with
//     occasional deflections, as a head sitting still with the odd movement.
//
// A seeded math/rand makes a run reproducible, and the clock is injectable so
// that tests can cover minutes of streaming instantly.
package synth

import (
	"context"
	"math"
	"math/rand"
	"time"

	"gomuse/internal/hub"
	"gomuse/internal/sample"
)

// Options configures a Source. The zero value is valid and gives a
// deterministic generator on the real clock starting now.
type Options struct {
	// Seed seeds the noise generator. Runs with the same seed, start and
	// stream set produce byte-identical samples.
	Seed int64
	// Start is the Unix timestamp of the first sample. Zero means "now".
	Start float64
	// Now returns the current Unix time in seconds. Nil means the real clock.
	Now func() float64
	// Sleep waits for d or until ctx is done, returning ctx.Err() if it was
	// cancelled. Nil means a real sleep. Tests supply a virtual clock here.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Source produces synthetic chunks for a set of streams.
type Source struct {
	hub   *hub.Hub
	gens  []*generator
	now   func() float64
	sleep func(context.Context, time.Duration) error
}

// New returns a Source publishing the selected streams to h with the default
// options. streams maps a stream ID to whether it is enabled; a missing key is
// treated as disabled.
func New(h *hub.Hub, streams map[sample.StreamID]bool) *Source {
	return NewWithOptions(h, streams, Options{})
}

// NewWithOptions is New with explicit options.
func NewWithOptions(h *hub.Hub, streams map[sample.StreamID]bool, opts Options) *Source {
	now := opts.Now
	if now == nil {
		now = realNow
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = realSleep
	}
	start := opts.Start
	if start <= 0 {
		start = now()
	}

	s := &Source{hub: h, now: now, sleep: sleep}
	// Iterate sample.All rather than the map so the generator order — and
	// therefore the tie-break between the two 52 Hz streams — is stable.
	for _, id := range sample.All() {
		if streams[id] {
			s.gens = append(s.gens, newGenerator(id, start, opts.Seed+int64(id)))
		}
	}
	return s
}

// Streams reports how many streams this source will publish.
func (s *Source) Streams() int { return len(s.gens) }

// Run publishes chunks until ctx is cancelled, then returns ctx.Err(). It
// paces itself against the clock: the packet whose last sample is due soonest
// is generated next, after sleeping until that instant.
func (s *Source) Run(ctx context.Context) error {
	if len(s.gens) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := s.gens[0]
		for _, g := range s.gens[1:] {
			if g.due() < next.due() {
				next = g
			}
		}
		wait := time.Duration((next.due() - s.now()) * float64(time.Second))
		if err := s.sleep(ctx, wait); err != nil {
			return err
		}
		s.hub.Publish(next.chunk())
	}
}

func realNow() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// Still yield, so a starved generator cannot spin the CPU.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// generator holds the per-stream waveform state.
type generator struct {
	meta  sample.Meta
	rng   *rand.Rand
	start float64
	index int64 // samples already emitted, per channel

	pink   []pinkNoise
	phase  []float64 // per-channel oscillator phase offset
	amp    []float64 // per-channel oscillator amplitude
	drift  []float64 // per-channel slow-drift amplitude
	dphase []float64 // per-channel drift phase offset
	burst  []float64 // per-channel decaying deflection (gyro)
}

func newGenerator(id sample.StreamID, start float64, seed int64) *generator {
	m := sample.MetaFor(id)
	n := m.NumChannels()
	g := &generator{
		meta:   m,
		rng:    rand.New(rand.NewSource(seed)),
		start:  start,
		pink:   make([]pinkNoise, n),
		phase:  make([]float64, n),
		amp:    make([]float64, n),
		drift:  make([]float64, n),
		dphase: make([]float64, n),
		burst:  make([]float64, n),
	}
	for c := 0; c < n; c++ {
		g.phase[c] = g.rng.Float64() * 2 * math.Pi
		g.dphase[c] = g.rng.Float64() * 2 * math.Pi
	}
	if id == sample.EEG {
		// Alpha amplitude in the tens of microvolts, different per channel.
		g.amp = []float64{28, 18, 22, 34, 12}
		// TP9 and TP10 wander like a poorly seated electrode; AF7/AF8 are
		// steadier; the AUX channel sits between. Hundreds of microvolts,
		// which is what makes the filter toggle worth having.
		g.drift = []float64{320, 60, 45, 180, 90}
	}
	return g
}

// due is the instant at which the last sample of the next packet exists, and
// therefore the earliest the packet could be delivered.
func (g *generator) due() float64 {
	return g.start + float64(g.index+int64(g.meta.SamplesPerPacket))*g.meta.Period()
}

// chunk generates the next packet and advances the sample counter.
func (g *generator) chunk() sample.Chunk {
	n := g.meta.SamplesPerPacket
	nch := g.meta.NumChannels()

	ts := make([]float64, n)
	data := make([][]float64, nch)
	for c := range data {
		data[c] = make([]float64, n)
	}

	for i := 0; i < n; i++ {
		idx := g.index + int64(i)
		// Elapsed seconds, not the absolute Unix time: at 1.7e9 an f64 has
		// only ~0.2 us of resolution left, which would quantise the phase.
		e := float64(idx) * g.meta.Period()
		ts[i] = g.start + e
		for c := 0; c < nch; c++ {
			data[c][i] = g.value(c, e)
		}
	}
	g.index += int64(n)

	return sample.Chunk{Stream: g.meta.ID, Timestamps: ts, Data: data}
}

const (
	alphaHz    = 10.0 // the classic occipital alpha rhythm
	driftHz    = 0.05 // ~20 s wander, well below the 0.5 Hz DC blocker corner
	pulseHz    = 1.05 // ~63 bpm
	eegNoiseUV = 9.0
)

// ppgPedestal is the raw DC level of each PPG channel, in device counts.
var ppgPedestal = [3]float64{1_650_000, 1_180_000, 820_000}

func (g *generator) value(c int, e float64) float64 {
	switch g.meta.ID {
	case sample.EEG:
		v := g.amp[c] * math.Sin(2*math.Pi*alphaHz*e+g.phase[c])
		v += eegNoiseUV * g.pink[c].next(g.rng)
		v += g.drift[c] * math.Sin(2*math.Pi*driftHz*e+g.dphase[c])
		return v

	case sample.PPG:
		base := ppgPedestal[c%len(ppgPedestal)]
		p := pulseShape(math.Mod(e*pulseHz+g.phase[c]/(2*math.Pi), 1))
		// A few percent modulation on the pedestal, as real PPG shows.
		return base * (1 + 0.045*p) * (1 + 0.0004*g.rng.NormFloat64())

	case sample.ACC:
		v := 0.004 * g.rng.NormFloat64()
		// A slow postural sway, different period per axis.
		v += 0.03 * math.Sin(2*math.Pi*(0.07+0.03*float64(c))*e+g.dphase[c])
		if c == 2 {
			v += 1.0 // gravity on Z with the band worn upright
		}
		return v

	case sample.Gyro:
		v := 0.8*g.rng.NormFloat64() + g.burst[c]
		g.burst[c] *= 0.985
		if g.rng.Float64() < 0.0015 {
			// A head turn: tens of degrees per second, decaying away.
			g.burst[c] += (g.rng.Float64()*2 - 1) * 45
		}
		return v
	}
	return 0
}

// pulseShape returns a normalised photoplethysmogram over one cardiac cycle:
// a systolic peak followed by a smaller dicrotic notch rebound.
func pulseShape(ph float64) float64 {
	if ph < 0 {
		ph += 1
	}
	systolic := math.Exp(-(ph - 0.14) * (ph - 0.14) / 0.004)
	dicrotic := 0.42 * math.Exp(-(ph-0.36)*(ph-0.36)/0.010)
	return systolic + dicrotic
}

// pinkNoise is Robert Bristow-Johnson's economical 1/f approximation: three
// one-pole filters over the same white source, summed. Close enough to pink
// across the EEG band for the trace to look right.
type pinkNoise struct{ b0, b1, b2 float64 }

func (p *pinkNoise) next(r *rand.Rand) float64 {
	w := r.NormFloat64()
	p.b0 = 0.99765*p.b0 + w*0.0990460
	p.b1 = 0.96300*p.b1 + w*0.2965164
	p.b2 = 0.57000*p.b2 + w*1.0526913
	// The 0.2 brings the result back to roughly unit RMS.
	return 0.2 * (p.b0 + p.b1 + p.b2 + w*0.1848)
}
