// Package record writes live sample chunks to CSV on disk.
//
// One file per stream, not one file for the recording: the four streams run at
// different rates (EEG at 256 Hz, PPG at 64, motion at 52), so a single joined
// table would be three quarters padding and would force a resampling decision
// on the recorder that the analysis tool is better placed to make.
//
// Files are opened lazily, on the first chunk of a stream that actually
// arrives, so a run with a preset that carries no PPG leaves no misleading
// empty ppg file behind. Every file is flushed and closed when Run returns,
// including on Ctrl-C, so an interrupted recording is still valid CSV.
package record

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"gomuse/internal/hub"
	"gomuse/internal/sample"
)

// DefaultBuffer is the subscription depth used for the recorder's hub channel.
//
// The hub drops chunks it cannot deliver, and for a recording a dropped chunk
// is lost data, unlike a dropped display frame. The buffer is therefore sized
// to ride out a multi-second disk or scheduler stall: at the combined ~55
// chunks per second of all four streams, 8192 chunks is over two minutes of
// slack.
const DefaultBuffer = 8192

// timeLayout is the filename stamp: sortable, and free of characters that need
// quoting in a shell.
const timeLayout = "2006-01-02_15-04-05"

// Config configures a Recorder.
type Config struct {
	// Dir is the output directory. It is created if absent; empty means the
	// current directory.
	Dir string
	// Streams selects which streams to record. A nil or empty map records
	// every stream.
	Streams map[sample.StreamID]bool
	// Now supplies the timestamp shared by every filename in the run. It is
	// injectable so tests get deterministic names; nil means time.Now.
	Now func() time.Time
}

// writer is one open output file.
type writer struct {
	f      *os.File
	csv    *csv.Writer
	path   string
	rows   int
	closed bool
}

// Recorder subscribes to a hub and writes each stream to its own CSV file.
// Its accessors are safe to call while Run is in flight.
type Recorder struct {
	dir     string
	stamp   string
	streams map[sample.StreamID]bool

	// subscribed is closed once Run has registered with the hub, so tests can
	// publish without racing the subscription.
	subscribed chan struct{}
	// buffer is the subscription depth; tests lower it to force drops.
	buffer int
	// onChunk, if non-nil, runs before each chunk is written. Test hook.
	onChunk func()

	mu        sync.Mutex
	writers   map[sample.StreamID]*writer
	order     []sample.StreamID
	malformed int
	dropped   uint64
	// row holds the reusable output record, sized to the widest stream.
	row []string
}

// New prepares a recorder, creating the output directory if needed. No CSV
// files are created until Run sees data for a stream.
func New(cfg Config) (*Recorder, error) {
	dir := cfg.Dir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("record: create output dir: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	streams := make(map[sample.StreamID]bool, len(cfg.Streams))
	for _, id := range sample.All() {
		if len(cfg.Streams) == 0 || cfg.Streams[id] {
			streams[id] = true
		}
	}

	widest := 0
	for _, id := range sample.All() {
		if n := sample.MetaFor(id).NumChannels(); n > widest {
			widest = n
		}
	}

	return &Recorder{
		dir:        dir,
		stamp:      now().Format(timeLayout),
		streams:    streams,
		subscribed: make(chan struct{}),
		buffer:     DefaultBuffer,
		writers:    make(map[sample.StreamID]*writer),
		row:        make([]string, 1+widest),
	}, nil
}

// Run subscribes to h and records until ctx is cancelled or the hub closes,
// then flushes and closes every open file before returning. Chunks already
// queued when ctx is cancelled are written rather than discarded: they are
// data the hub has already promised us.
//
// A cancelled context is not an error; Run returns non-nil only if writing
// failed. Run is single use.
func (r *Recorder) Run(ctx context.Context, h *hub.Hub) (err error) {
	// SubscribeSub, not Subscribe: the hub-wide drop counter also counts the
	// browser clients, which drop display frames routinely and by design. A
	// recording that lost nothing must not inherit their number, or the
	// "this recording may have holes" warning fires on every run with a
	// viewer open and stops meaning anything.
	ch, sub, cancel := h.SubscribeSub(r.buffer)
	defer cancel()

	close(r.subscribed)

	defer func() {
		r.mu.Lock()
		r.dropped = sub.Dropped()
		r.mu.Unlock()
		if cerr := r.closeAll(); err == nil {
			err = cerr
		}
	}()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case c, ok := <-ch:
			if !ok {
				return nil
			}
			if err := r.handle(c); err != nil {
				return err
			}
		}
	}

	// Drain whatever the hub already handed us before shutting down.
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return nil
			}
			if err := r.handle(c); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// handle writes one chunk, skipping and counting anything malformed rather
// than emitting ragged rows.
func (r *Recorder) handle(c sample.Chunk) error {
	if r.onChunk != nil {
		r.onChunk()
	}
	if !r.streams[c.Stream] {
		return nil
	}
	m := sample.MetaFor(c.Stream)
	n := c.NumSamples()

	if c.NumChannels() != m.NumChannels() {
		r.countMalformed()
		return nil
	}
	for _, col := range c.Data {
		if len(col) < n {
			r.countMalformed()
			return nil
		}
	}
	if n == 0 {
		// Nothing to write, and not a reason to create the file.
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	w, err := r.writerFor(m)
	if err != nil {
		return err
	}
	row := r.row[:1+m.NumChannels()]
	for i := 0; i < n; i++ {
		// Full precision to the microsecond: the default float formatting
		// would switch to exponent notation at these magnitudes and silently
		// cost the recording its sample-level resolution.
		row[0] = strconv.FormatFloat(c.Timestamps[i], 'f', 6, 64)
		for ch := range c.Data {
			row[1+ch] = strconv.FormatFloat(c.Data[ch][i], 'f', -1, 64)
		}
		if err := w.csv.Write(row); err != nil {
			return fmt.Errorf("record: write %s: %w", w.path, err)
		}
		w.rows++
	}
	// Flush per chunk so an interrupted or crashed run loses at most the
	// chunk in hand.
	w.csv.Flush()
	if err := w.csv.Error(); err != nil {
		return fmt.Errorf("record: write %s: %w", w.path, err)
	}
	return nil
}

// writerFor returns the open writer for a stream, creating the file and
// writing its header on first use. Caller holds r.mu.
func (r *Recorder) writerFor(m sample.Meta) (*writer, error) {
	if w, ok := r.writers[m.ID]; ok {
		return w, nil
	}
	path := filepath.Join(r.dir, fmt.Sprintf("%s_%s.csv", m.Name, r.stamp))
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("record: create %s: %w", path, err)
	}
	w := &writer{f: f, csv: csv.NewWriter(f), path: path}

	header := append([]string{"timestamp"}, m.Channels...)
	if err := w.csv.Write(header); err != nil {
		f.Close()
		return nil, fmt.Errorf("record: write header %s: %w", path, err)
	}
	r.writers[m.ID] = w
	r.order = append(r.order, m.ID)
	return w, nil
}

func (r *Recorder) countMalformed() {
	r.mu.Lock()
	r.malformed++
	r.mu.Unlock()
}

// closeAll flushes and closes every open file, reporting the first failure but
// always attempting all of them.
func (r *Recorder) closeAll() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var firstErr error
	for _, id := range r.order {
		w := r.writers[id]
		if w == nil || w.closed {
			continue
		}
		w.csv.Flush()
		if err := w.csv.Error(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("record: flush %s: %w", w.path, err)
		}
		if err := w.f.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("record: close %s: %w", w.path, err)
		}
		w.closed = true
	}
	return firstErr
}

// Files returns the paths written so far, in the order the streams first
// appeared.
func (r *Recorder) Files() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, filepath.Join(r.dir, fmt.Sprintf("%s_%s.csv", sample.MetaFor(id).Name, r.stamp)))
	}
	return out
}

// Rows returns the number of sample rows written per stream, excluding
// headers. Streams that never produced a file are absent.
func (r *Recorder) Rows() map[sample.StreamID]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[sample.StreamID]int, len(r.order))
	for _, id := range r.order {
		if w := r.writers[id]; w != nil {
			out[id] = w.rows
		}
	}
	return out
}

// Dropped returns how many chunks this recorder missed during the run. It is
// this subscriber's own count, not the hub-wide total, so it is exact: any
// non-zero value means this recording really does have holes and the user
// should be warned.
func (r *Recorder) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Malformed returns how many chunks were skipped because their shape did not
// match the stream's metadata.
func (r *Recorder) Malformed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.malformed
}
