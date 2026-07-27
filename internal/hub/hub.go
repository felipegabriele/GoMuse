// Package hub fans one producer's sample chunks out to many independent
// consumers: the WebSocket clients and the CSV recorder.
//
// The producer is the BLE notification callback. It must never be made to wait
// on a consumer — stalling it would back pressure into the Bluetooth stack and
// cost us samples from the headband itself, which is unrecoverable. So every
// subscriber gets its own buffered channel and, when that buffer is full, the
// chunk is dropped for that subscriber alone and counted.
//
// The drop policy is drop-newest: the chunk that cannot fit is discarded and
// the backlog already queued is left alone. Dropping display frames is correct
// here — a live viewer wants recent data, not a complete history — and
// drop-newest is the policy that cannot lose a chunk a consumer has already
// been promised, which matters for the CSV recorder: give it a generous buffer
// and it never drops, whereas drop-oldest would silently punch holes in a
// recording under a momentary disk stall. A viewer briefly behind sees a small
// gap and then catches up, because the queue ahead of it is bounded and short.
package hub

import (
	"sync"
	"sync/atomic"

	"gomuse/internal/sample"
)

type subscriber struct {
	ch      chan sample.Chunk
	dropped atomic.Uint64
}

// Sub is a handle to one subscription, exposing that subscriber's own drop
// count.
//
// The hub-wide Dropped total cannot answer "did *I* lose data?", because a
// browser tab with a short buffer drops routinely by design. A recorder that
// reported the hub-wide figure would warn about holes on nearly every run that
// had a viewer attached, which trains the user to ignore the warning. Per
// subscriber counts keep it meaningful.
type Sub struct {
	s *subscriber
}

// Dropped returns the chunks this subscriber alone missed.
func (s Sub) Dropped() uint64 { return s.s.dropped.Load() }

// Hub is a broker between one producer and N subscribers. All methods are safe
// for concurrent use.
type Hub struct {
	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	closed bool

	dropped atomic.Uint64
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{subs: make(map[*subscriber]struct{})}
}

// Publish delivers c to every current subscriber. It never blocks: a
// subscriber whose buffer is full misses this chunk and the drop is counted.
// Publishing to a closed hub is a no-op.
func (h *Hub) Publish(c sample.Chunk) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for s := range h.subs {
		select {
		case s.ch <- c:
		default:
			h.dropped.Add(1)
			s.dropped.Add(1)
		}
	}
}

// Subscribe registers a new consumer and returns its channel along with a
// cancel func that unsubscribes it and closes the channel. Only chunks
// published after the call are delivered; there is no history.
//
// buffer is the number of chunks that may queue before this subscriber starts
// dropping. A negative buffer is treated as 0 (every chunk dropped unless a
// reader happens to be waiting).
//
// cancel is idempotent and safe to call from any goroutine. Callers should
// defer it.
func (h *Hub) Subscribe(buffer int) (<-chan sample.Chunk, func()) {
	ch, _, cancel := h.SubscribeSub(buffer)
	return ch, cancel
}

// SubscribeSub is Subscribe plus a handle for this subscriber's own drop
// count. Use it wherever dropped chunks mean lost data rather than a skipped
// display frame.
func (h *Hub) SubscribeSub(buffer int) (<-chan sample.Chunk, Sub, func()) {
	if buffer < 0 {
		buffer = 0
	}
	s := &subscriber{ch: make(chan sample.Chunk, buffer)}

	h.mu.Lock()
	if h.closed {
		// The hub is already shut down: hand back a closed channel so the
		// caller's range loop terminates immediately instead of hanging.
		h.mu.Unlock()
		close(s.ch)
		return s.ch, Sub{s}, func() {}
	}
	h.subs[s] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subs[s]; !ok {
				// Close already removed and closed it.
				return
			}
			delete(h.subs, s)
			close(s.ch)
		})
	}
	return s.ch, Sub{s}, cancel
}

// Dropped returns the total number of chunk deliveries dropped across all
// subscribers since the hub was created. One slow subscriber missing one chunk
// counts as one drop. For "did this consumer lose data", use Sub.Dropped
// instead — see the note on Sub.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }

// Close shuts the hub down, closing every subscriber channel. Later Publish
// calls are no-ops and Close itself is idempotent.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		close(s.ch)
	}
}
