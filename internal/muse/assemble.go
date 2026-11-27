package muse

import (
	"sync"

	"gomuse/internal/sample"
)

// maxPending bounds how many partially-filled packets are held while waiting
// for their remaining channels. The headband interleaves its per-channel
// notifications tightly, so in normal operation at most one packet is ever
// open; the slack absorbs reordering, and the cap means a channel that stops
// notifying leaks nothing.
const maxPending = 4

// partial is one packet index's worth of channels, filling up as the
// per-channel notifications arrive.
type partial struct {
	data [][]float64
	got  int
	// arrival is the earliest arrival across the channels seen so far. The
	// least-delayed observation is the least biased one, which is what the
	// dejitter wants.
	arrival float64
}

// assembler reassembles a multi-characteristic stream (EEG or PPG) into whole
// chunks.
//
// muse-lsl keys this off GATT handles and simply assumes a fixed arrival order,
// publishing when the last handle in that order shows up. CoreBluetooth does
// not expose handles at all, so we key off the channel index and publish once
// every channel of a packet has arrived — which also survives reordering,
// where the fixed-order assumption would silently mispair samples.
type assembler struct {
	stream  sample.StreamID
	nchan   int
	publish func(sample.Chunk)
	touch   func()

	meta sample.Meta

	mu sync.Mutex
	// dej is created from the first completed packet rather than at
	// construction. Its intercept is fixed for the life of the stream, so
	// anchoring it on a real arrival keeps the connect-to-first-packet delay
	// out of every timestamp — and means the fit never sees an arrival from a
	// different epoch than its anchor, which would drive the slope negative.
	dej     *Dejitter
	pending map[uint16]*partial
	// order records pending insertion order so the oldest can be evicted.
	order []uint16
}

func newAssembler(id sample.StreamID, publish func(sample.Chunk), touch func()) *assembler {
	meta := sample.MetaFor(id)
	return &assembler{
		stream:  id,
		nchan:   meta.NumChannels(),
		meta:    meta,
		publish: publish,
		touch:   touch,
		pending: make(map[uint16]*partial, maxPending),
	}
}

// add records one channel of one packet, and publishes the chunk once the
// packet is complete. It is safe to call concurrently from the several BLE
// callback goroutines feeding one stream.
func (a *assembler) add(tm uint16, channel int, samples []float64, arrival float64) {
	if channel < 0 || channel >= a.nchan {
		return
	}
	if a.touch != nil {
		a.touch()
	}

	a.mu.Lock()

	p, ok := a.pending[tm]
	if !ok {
		p = &partial{data: make([][]float64, a.nchan), arrival: arrival}
		a.pending[tm] = p
		a.order = append(a.order, tm)
		a.evict()
	}

	if p.data[channel] != nil {
		// A repeat of a channel we already have means the previous packet at
		// this index will never complete. Start it over rather than pairing
		// samples from two different packets.
		p.data = make([][]float64, a.nchan)
		p.got = 0
		p.arrival = arrival
	}

	p.data[channel] = samples
	p.got++
	if arrival < p.arrival {
		p.arrival = arrival
	}

	if p.got < a.nchan {
		a.mu.Unlock()
		return
	}

	a.remove(tm)
	if a.dej == nil {
		a.dej = NewDejitter(a.meta.Rate, a.meta.SamplesPerPacket, p.arrival)
	}
	// Next must run under the lock: it advances the shared time base, and
	// concurrent callers would otherwise interleave sample indices.
	ts := a.dej.Next(tm, p.arrival)
	chunk := sample.Chunk{Stream: a.stream, Timestamps: ts, Data: p.data}
	a.mu.Unlock()

	// Publish outside the lock; the hub is non-blocking, but there is no
	// reason to hold up the other channels' callbacks behind it.
	a.publish(chunk)
}

// evict drops the oldest pending packet when the buffer is over its cap. Called
// with the lock held.
func (a *assembler) evict() {
	for len(a.order) > maxPending {
		oldest := a.order[0]
		a.order = a.order[1:]
		delete(a.pending, oldest)
	}
}

// remove takes a completed packet out of the pending set. Called with the lock
// held.
func (a *assembler) remove(tm uint16) {
	delete(a.pending, tm)
	for i, v := range a.order {
		if v == tm {
			a.order = append(a.order[:i], a.order[i+1:]...)
			return
		}
	}
}

// timing reports how this stream's timestamps were produced, and whether any
// packet has completed yet.
func (a *assembler) timing() (StreamTiming, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dej == nil {
		return StreamTiming{}, false
	}
	return a.dej.timing(a.meta.Name, a.meta.Rate), true
}
