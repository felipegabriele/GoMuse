package hub

import (
	"sync"
	"testing"
	"time"

	"gomuse/internal/sample"
)

// chunkN builds a tiny identifiable chunk.
func chunkN(n int) sample.Chunk {
	return sample.Chunk{
		Stream:     sample.EEG,
		Timestamps: []float64{float64(n)},
		Data:       [][]float64{{float64(n)}},
	}
}

// seq extracts the identifying number put in by chunkN.
func seq(c sample.Chunk) int { return int(c.Timestamps[0]) }

// recv reads one chunk or fails the test rather than hanging forever.
func recv(t *testing.T, ch <-chan sample.Chunk) sample.Chunk {
	t.Helper()
	select {
	case c, ok := <-ch:
		if !ok {
			t.Fatal("channel closed, want a chunk")
		}
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a chunk")
		return sample.Chunk{}
	}
}

func TestTwoSubscribersReceiveEveryChunkInOrder(t *testing.T) {
	h := New()
	defer h.Close()

	const n = 20
	a, cancelA := h.Subscribe(n)
	defer cancelA()
	b, cancelB := h.Subscribe(n)
	defer cancelB()

	for i := 0; i < n; i++ {
		h.Publish(chunkN(i))
	}
	for i := 0; i < n; i++ {
		if got := seq(recv(t, a)); got != i {
			t.Fatalf("subscriber a: got %d, want %d", got, i)
		}
		if got := seq(recv(t, b)); got != i {
			t.Fatalf("subscriber b: got %d, want %d", got, i)
		}
	}
	if d := h.Dropped(); d != 0 {
		t.Errorf("Dropped = %d, want 0", d)
	}
}

func TestStalledSubscriberDoesNotBlockProducer(t *testing.T) {
	h := New()
	defer h.Close()

	// Subscribe with a small buffer and deliberately never read from it.
	stalled, cancel := h.Subscribe(2)
	defer cancel()
	_ = stalled

	// A healthy subscriber alongside it must still get served.
	const n = 500
	healthy, cancelH := h.Subscribe(n)
	defer cancelH()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			h.Publish(chunkN(i))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a stalled subscriber")
	}

	if d := h.Dropped(); d == 0 {
		t.Fatal("Dropped = 0, want non-zero: the stalled subscriber should have lost chunks")
	}
	for i := 0; i < n; i++ {
		if got := seq(recv(t, healthy)); got != i {
			t.Fatalf("healthy subscriber: got %d, want %d", got, i)
		}
	}
}

func TestCancelStopsDeliveryAndClosesChannel(t *testing.T) {
	h := New()
	defer h.Close()

	ch, cancel := h.Subscribe(4)
	h.Publish(chunkN(1))
	if got := seq(recv(t, ch)); got != 1 {
		t.Fatalf("got %d, want 1", got)
	}

	cancel()
	h.Publish(chunkN(2))

	select {
	case c, ok := <-ch:
		if ok {
			t.Fatalf("received chunk %d after cancel, want closed channel", seq(c))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel was not closed by cancel")
	}

	cancel() // must be idempotent, must not panic or close twice
	cancel()
}

func TestLateSubscriberGetsOnlySubsequentChunks(t *testing.T) {
	h := New()
	defer h.Close()

	h.Publish(chunkN(1))
	h.Publish(chunkN(2))

	ch, cancel := h.Subscribe(4)
	defer cancel()

	h.Publish(chunkN(3))
	if got := seq(recv(t, ch)); got != 3 {
		t.Fatalf("got %d, want 3: a late subscriber must not see history", got)
	}
}

func TestCloseClosesAllSubscriberChannels(t *testing.T) {
	h := New()
	a, _ := h.Subscribe(1)
	b, _ := h.Subscribe(1)

	h.Close()

	for i, ch := range []<-chan sample.Chunk{a, b} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("subscriber %d: got a chunk, want closed channel", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d: channel not closed by Close", i)
		}
	}
	// Publishing after Close must not panic (send on closed channel).
	h.Publish(chunkN(1))
	h.Close() // idempotent
}

func TestConcurrentPublishSubscribeCancel(t *testing.T) {
	h := New()
	defer h.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				h.Publish(chunkN(i))
			}
		}()
	}

	for s := 0; s < 8; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ch, cancel := h.Subscribe(4)
				for i := 0; i < 3; i++ {
					select {
					case <-ch:
					case <-time.After(10 * time.Millisecond):
					}
				}
				cancel()
				cancel()
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	_ = h.Dropped()
}

// TestPerSubscriberDropsAreIsolated is the reason SubscribeSub exists: a
// viewer with a short buffer drops constantly by design, and that must not be
// attributed to a recorder that lost nothing.
func TestPerSubscriberDropsAreIsolated(t *testing.T) {
	h := New()
	defer h.Close()

	// A stalled viewer with almost no buffer.
	_, viewer, cancelViewer := h.SubscribeSub(1)
	defer cancelViewer()

	// A recorder with room to spare, drained promptly.
	recCh, recorder, cancelRec := h.SubscribeSub(256)
	defer cancelRec()

	const n = 100
	done := make(chan int)
	go func() {
		count := 0
		for range recCh {
			count++
			if count == n {
				break
			}
		}
		done <- count
	}()

	for i := 0; i < n; i++ {
		h.Publish(sample.Chunk{Stream: sample.EEG})
	}

	select {
	case got := <-done:
		if got != n {
			t.Fatalf("recorder received %d chunks, want %d", got, n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the recorder to drain")
	}

	if recorder.Dropped() != 0 {
		t.Errorf("recorder dropped %d, want 0", recorder.Dropped())
	}
	if viewer.Dropped() == 0 {
		t.Error("viewer dropped 0, expected the stalled subscriber to drop")
	}
	// The hub-wide figure lumps them together, which is exactly why it cannot
	// answer "did the recording lose data".
	if h.Dropped() != viewer.Dropped() {
		t.Errorf("hub total %d != viewer drops %d", h.Dropped(), viewer.Dropped())
	}
}
