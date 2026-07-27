package muse

import (
	"sync"
	"testing"

	"gomuse/internal/sample"
)

// collect returns an assembler that appends published chunks to a slice.
func collect() (*assembler, *[]sample.Chunk, *sync.Mutex) {
	var (
		mu   sync.Mutex
		got  []sample.Chunk
		asm  *assembler
		push = func(c sample.Chunk) {
			mu.Lock()
			got = append(got, c)
			mu.Unlock()
		}
	)
	asm = newAssembler(sample.EEG, push, nil)
	return asm, &got, &mu
}

// chanSamples builds a distinguishable ramp for one channel.
func chanSamples(channel, n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = float64(channel*100 + i)
	}
	return s
}

// TestAssemblerPublishesWhenComplete: nothing is emitted until the last
// channel of a packet arrives, and then exactly one chunk is.
func TestAssemblerPublishesWhenComplete(t *testing.T) {
	asm, got, mu := collect()
	n := sample.MetaFor(sample.EEG).SamplesPerPacket

	for ch := 0; ch < sample.MetaFor(sample.EEG).NumChannels(); ch++ {
		mu.Lock()
		count := len(*got)
		mu.Unlock()
		if count != 0 {
			t.Fatalf("published after %d of %d channels", ch, asm.nchan)
		}
		asm.add(7, ch, chanSamples(ch, n), float64(ch)*0.001)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(*got))
	}
	c := (*got)[0]
	if c.Stream != sample.EEG {
		t.Errorf("stream = %v, want EEG", c.Stream)
	}
	if c.NumChannels() != 5 || c.NumSamples() != n {
		t.Fatalf("chunk is %dx%d, want 5x%d", c.NumChannels(), c.NumSamples(), n)
	}
	// Channels must land at their own index, not in arrival order.
	for ch := 0; ch < c.NumChannels(); ch++ {
		if c.Data[ch][0] != float64(ch*100) {
			t.Errorf("channel %d starts at %v, want %v", ch, c.Data[ch][0], float64(ch*100))
		}
	}
}

// TestAssemblerOutOfOrderChannels: the headband's real arrival order is not
// channel order, and muse-lsl's "wait for a specific handle" would mispair
// samples if it ever changed. Completion-based assembly must not care.
func TestAssemblerOutOfOrderChannels(t *testing.T) {
	asm, got, mu := collect()
	n := sample.MetaFor(sample.EEG).SamplesPerPacket

	for _, ch := range []int{4, 3, 2, 0, 1} {
		asm.add(11, ch, chanSamples(ch, n), 0)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(*got))
	}
	for ch, col := range (*got)[0].Data {
		if col[0] != float64(ch*100) {
			t.Errorf("channel %d holds channel %v's data", ch, col[0]/100)
		}
	}
}

// TestAssemblerInterleavedPackets: two packets in flight at once must not have
// their samples mixed together.
func TestAssemblerInterleavedPackets(t *testing.T) {
	asm, got, mu := collect()
	n := sample.MetaFor(sample.EEG).SamplesPerPacket

	// Fill packet 1 and packet 2 alternately, completing 2 first.
	for ch := 0; ch < 5; ch++ {
		asm.add(1, ch, chanSamples(ch, n), 0)
		if ch < 4 {
			asm.add(2, ch, chanSamples(ch+10, n), 0)
		}
	}
	mu.Lock()
	if len(*got) != 1 {
		mu.Unlock()
		t.Fatalf("got %d chunks, want 1 (packet 1 complete)", len(*got))
	}
	mu.Unlock()

	asm.add(2, 4, chanSamples(14, n), 0)

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("got %d chunks, want 2", len(*got))
	}
	if (*got)[0].Data[0][0] != 0 {
		t.Errorf("first chunk carries packet 2's data")
	}
	if (*got)[1].Data[0][0] != 1000 {
		t.Errorf("second chunk = %v, want packet 2's data", (*got)[1].Data[0][0])
	}
}

// TestAssemblerEvictsStalePartials: if one channel stops notifying, the
// incomplete packets must not accumulate without bound.
func TestAssemblerEvictsStalePartials(t *testing.T) {
	asm, got, mu := collect()
	n := sample.MetaFor(sample.EEG).SamplesPerPacket

	for tm := 0; tm < 100; tm++ {
		// Channel 4 never arrives, so nothing ever completes.
		for ch := 0; ch < 4; ch++ {
			asm.add(uint16(tm), ch, chanSamples(ch, n), 0)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Errorf("published %d chunks from incomplete packets", len(*got))
	}
	asm.mu.Lock()
	defer asm.mu.Unlock()
	if len(asm.pending) > maxPending {
		t.Errorf("pending grew to %d, want <= %d", len(asm.pending), maxPending)
	}
	if len(asm.order) != len(asm.pending) {
		t.Errorf("order (%d) and pending (%d) disagree", len(asm.order), len(asm.pending))
	}
}

// TestAssemblerTimestampsIncrease: successive chunks must not overlap in time.
func TestAssemblerTimestampsIncrease(t *testing.T) {
	asm, got, mu := collect()
	n := sample.MetaFor(sample.EEG).SamplesPerPacket

	for tm := 1; tm <= 20; tm++ {
		for ch := 0; ch < 5; ch++ {
			asm.add(uint16(tm), ch, chanSamples(ch, n), float64(tm)*float64(n)/256)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 20 {
		t.Fatalf("got %d chunks, want 20", len(*got))
	}
	last := -1.0
	for i, c := range *got {
		for j, ts := range c.Timestamps {
			if ts <= last {
				t.Fatalf("chunk %d sample %d: timestamp %v not after %v", i, j, ts, last)
			}
			last = ts
		}
	}
}

// TestAssemblerConcurrent gives the race detector something to chew on: five
// goroutines, one per channel, as the real BLE callbacks are.
//
// The goroutines are held to the same packet by a barrier. That is not
// test-rigging: the headband emits all five channels of a packet back-to-back,
// so channels never drift more than a packet or so apart. Letting them run
// hundreds of packets ahead of each other would only exercise eviction, and
// would say nothing about whether concurrent assembly pairs samples correctly.
func TestAssemblerConcurrent(t *testing.T) {
	asm, got, mu := collect()
	n := sample.MetaFor(sample.EEG).SamplesPerPacket
	const packets = 200
	const channels = 5

	var wg sync.WaitGroup
	feed := make([]chan uint16, channels)
	acked := make(chan struct{}, channels)

	for ch := 0; ch < channels; ch++ {
		feed[ch] = make(chan uint16)
		wg.Add(1)
		go func(ch int, in <-chan uint16) {
			defer wg.Done()
			for tm := range in {
				asm.add(tm, ch, chanSamples(ch, n), float64(tm))
				acked <- struct{}{}
			}
		}(ch, feed[ch])
	}

	// Hand every channel the same packet, then wait for all five to land
	// before moving on. The sends are unbuffered, so the workers genuinely
	// run concurrently within a packet.
	for tm := 1; tm <= packets; tm++ {
		for ch := range feed {
			feed[ch] <- uint16(tm)
		}
		for i := 0; i < channels; i++ {
			<-acked
		}
	}
	for ch := range feed {
		close(feed[ch])
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != packets {
		t.Fatalf("got %d chunks, want %d", len(*got), packets)
	}
	for i, c := range *got {
		for ch, col := range c.Data {
			if col == nil {
				t.Fatalf("chunk %d channel %d is nil", i, ch)
			}
			if col[0] != float64(ch*100) {
				t.Fatalf("chunk %d channel %d holds the wrong channel's data", i, ch)
			}
		}
	}
}
