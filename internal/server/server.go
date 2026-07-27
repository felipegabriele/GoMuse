// Package server serves the live viewer: the embedded single-page app over
// HTTP, and the sample stream over a WebSocket.
//
// Everything the browser needs is compiled into the binary with //go:embed, so
// gomuse is a single file with no runtime network access — the uPlot build in
// web/vendor is deliberately vendored rather than pulled from a CDN.
//
// Each WebSocket client is independent. It gets its own hub subscription (so a
// slow browser drops its own frames and nobody else's) and its own filter
// state: display filtering is a per-viewer preference, and the DSP banks carry
// per-channel history that cannot be shared. Because the same sample.Chunk is
// handed to every subscriber, filtering must never write into the chunk it was
// given; dsp.Bank.Process and dsp.DCBlockerBank.Process both return fresh
// slices, which is what makes that safe.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"gomuse/internal/dsp"
	"gomuse/internal/hub"
	"gomuse/internal/pulse"
	"gomuse/internal/sample"
	"gomuse/internal/wire"
)

//go:embed web
var webFS embed.FS

const (
	// subscriberBuffer is how many chunks may queue for one browser before it
	// starts dropping. Roughly a second of all four streams: enough to ride
	// out a GC pause or a tab switch, short enough that a viewer that falls
	// behind catches up quickly instead of showing stale data.
	subscriberBuffer = 96

	// writeTimeout bounds a single frame write. A browser that has stopped
	// reading without closing the TCP connection must not pin a goroutine.
	writeTimeout = 10 * time.Second

	// shutdownTimeout bounds the graceful shutdown.
	shutdownTimeout = 5 * time.Second

	// Display filter parameters, matching muse-lsl's viewer: a 32-tap
	// 1-40 Hz bandpass at the EEG rate, preceded by a 0.5 Hz DC blocker
	// because that FIR has a DC gain of 0.87 and does not remove drift on
	// its own. See internal/dsp.
	filterTaps = 32
	filterLow  = 1.0
	filterHigh = 40.0

	// pulseIntervalSec throttles the pulse message. Four a second is far more
	// often than a heart beats; it exists so the readout feels live rather
	// than to carry new beats.
	pulseIntervalSec = 0.25
)

// Config configures a Server.
type Config struct {
	// Addr is the TCP address to listen on, e.g. ":1111".
	Addr string
	// Hub is the source of sample chunks. Required.
	Hub *hub.Hub
	// Streams describes the connected headband's streams. The viewer builds
	// its entire UI from this, so it must match what the source publishes: an
	// Athena's EEG is 4 channels where a classic headband's is 5. Nil means
	// the classic layout.
	Streams *sample.Set
}

// Server serves the viewer app and the sample WebSocket.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	codec   *wire.Codec
	clients atomic.Int64

	// shutdown is closed when ListenAndServe's context is cancelled, which
	// ends the WebSocket loops. http.Server.Shutdown cannot do that itself:
	// a hijacked connection is invisible to it.
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

// StreamInfo is the per-stream metadata sent to a client on connect, so that
// the browser does not hardcode channel names or rates.
type StreamInfo struct {
	ID               int      `json:"id"`
	Name             string   `json:"name"`
	Channels         []string `json:"channels"`
	Unit             string   `json:"unit"`
	Rate             float64  `json:"rate"`
	SamplesPerPacket int      `json:"samplesPerPacket"`
}

// Hello is the first message on a WebSocket, sent as JSON text. Every later
// message is a binary wire frame.
type Hello struct {
	Type    string       `json:"type"`
	Streams []StreamInfo `json:"streams"`
}

// PulseMessage is the derived heart-rate message, sent as a WebSocket text
// frame alongside the binary sample frames.
//
// The numeric fields are pointers so that "not known yet" marshals as null
// rather than as zero: a bpm of 0 and an unknown bpm must not be confusable in
// the browser.
type PulseMessage struct {
	Type      string    `json:"type"`
	Channel   *string   `json:"channel"`
	State     string    `json:"state"`
	BPM       *float64  `json:"bpm"`
	RMSSD     *float64  `json:"rmssd"`
	IBIMs     *float64  `json:"ibiMs"`
	Beats     []float64 `json:"beats"`
	BeatCount int       `json:"beatCount"`
}

// Health is the /healthz body.
type Health struct {
	Status  string `json:"status"`
	Clients int64  `json:"clients"`
	Dropped uint64 `json:"dropped"`
}

// New returns a Server for cfg. It panics if cfg.Hub is nil, which is a
// programming error rather than a runtime condition.
func New(cfg Config) *Server {
	if cfg.Hub == nil {
		panic("server: Config.Hub is required")
	}
	if cfg.Streams == nil {
		cfg.Streams = sample.Classic()
	}
	s := &Server{
		cfg:      cfg,
		mux:      http.NewServeMux(),
		shutdown: make(chan struct{}),
		codec:    wire.NewCodec(cfg.Streams),
	}

	root, err := fs.Sub(webFS, "web")
	if err != nil {
		// Impossible: the directory is embedded at build time.
		panic("server: embedded web assets missing: " + err.Error())
	}

	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /ws", s.handleWS)
	s.mux.Handle("GET /", http.FileServerFS(root))
	return s
}

// Handler returns the HTTP handler, for httptest or for mounting elsewhere.
func (s *Server) Handler() http.Handler { return s.mux }

// Clients returns the number of connected WebSocket clients.
func (s *Server) Clients() int64 { return s.clients.Load() }

// ListenAndServe serves until ctx is cancelled, then shuts down gracefully and
// returns nil. A listen or serve failure is returned as-is.
func (s *Server) ListenAndServe(ctx context.Context) error {
	hs := &http.Server{
		Addr:    s.cfg.Addr,
		Handler: s.mux,
		// No WriteTimeout: it would cap the lifetime of every WebSocket.
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	s.shutdownOnce.Do(func() { close(s.shutdown) })

	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := hs.Shutdown(sctx)
	<-errc
	return err
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(Health{
		Status:  "ok",
		Clients: s.clients.Load(),
		Dropped: s.cfg.Hub.Dropped(),
	})
}

// hello builds the metadata message for a new client.
func hello(streams *sample.Set) Hello {
	h := Hello{Type: "hello"}
	for _, id := range streams.All() {
		m := streams.MetaFor(id)
		h.Streams = append(h.Streams, StreamInfo{
			ID:               int(id),
			Name:             m.Name,
			Channels:         m.Channels,
			Unit:             m.Unit,
			Rate:             m.Rate,
			SamplesPerPacket: m.SamplesPerPacket,
		})
	}
	return h
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The payload is already-compact float32; deflate would burn CPU 21
		// times a second per stream for nothing.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept has already written the error response.
	}
	defer func() {
		_ = conn.CloseNow()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// A server shutdown must end this connection too.
	go func() {
		select {
		case <-s.shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()

	s.clients.Add(1)
	defer s.clients.Add(-1)

	msg, err := json.Marshal(hello(s.cfg.Streams))
	if err != nil {
		return
	}
	wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
	err = conn.Write(wctx, websocket.MessageText, msg)
	wcancel()
	if err != nil {
		return
	}

	chunks, unsubscribe := s.cfg.Hub.Subscribe(subscriberBuffer)
	defer unsubscribe()

	cl := newClient(s.cfg.Streams)
	go cl.readLoop(ctx, cancel, conn)

	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-chunks:
			if !ok {
				// The hub closed: the capture is over.
				_ = conn.Close(websocket.StatusNormalClosure, "stream ended")
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(wctx, websocket.MessageBinary, s.codec.EncodeChunk(cl.prepare(c)))
			wcancel()
			if err != nil {
				return
			}

			if msg, due := cl.pulseUpdate(c); due {
				b, merr := json.Marshal(msg)
				if merr != nil {
					continue // A message we cannot encode is not worth dropping the viewer for.
				}
				wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
				err := conn.Write(wctx, websocket.MessageText, b)
				wcancel()
				if err != nil {
					return
				}
			}
		}
	}
}

// client is the per-connection state: the requested settings, and the DSP
// banks that implement them.
type client struct {
	// want is set by the reader goroutine and read by the writer goroutine.
	want atomic.Bool
	// window is the viewer's display window in seconds. The server does not
	// use it — the ring buffers live in the browser — but accepting it keeps
	// the control message single-purpose and lets a future recorder or
	// downsampler act on it.
	window atomic.Uint64

	// Everything below belongs to the writer goroutine alone.
	on   bool
	bank *dsp.Bank
	dc   *dsp.DCBlockerBank

	// Pulse tracking, writer-goroutine-owned like the DSP banks above.
	// tracker is nil when the device has no pulse stream at all.
	tracker    *pulse.Tracker
	pulseID    sample.StreamID
	pulseNames []string
	nextPulse  float64
}

// newClient sizes the per-viewer DSP to the connected headband's EEG stream:
// the filter banks carry one history per channel, so a 4-channel Athena and a
// 5-channel classic headband need differently shaped state.
func newClient(streams *sample.Set) *client {
	m, ok := streams.Meta(sample.EEG)
	if !ok {
		// No EEG on this device: the banks are never used, but they must not
		// be nil, and a zero-channel bank is valid.
		m = sample.Meta{Rate: 256}
	}
	nch, rate := m.NumChannels(), m.Rate
	c := &client{
		bank: dsp.NewBank(dsp.FirwinBandpass(filterTaps, filterLow, filterHigh, rate), nch),
		dc:   dsp.NewDCBlockerBank(0.5, rate, nch),
	}

	// Optics and PPG are alternatives — an Athena has the first, a classic
	// headband the second, and no device produces both — so the first one
	// present is this device's pulse stream.
	for _, id := range []sample.StreamID{sample.Optics, sample.PPG} {
		pm, ok := streams.Meta(id)
		if !ok {
			continue
		}
		c.tracker = pulse.NewTracker(pm.Rate, pm.NumChannels())
		c.pulseID = id
		c.pulseNames = pm.Channels
		break
	}
	return c
}

// pulseUpdate feeds a chunk to the pulse tracker and reports whether a message
// is due. It returns false for chunks of any other stream, for devices with no
// pulse stream, and between throttle intervals.
func (c *client) pulseUpdate(chunk sample.Chunk) (PulseMessage, bool) {
	if c.tracker == nil || chunk.Stream != c.pulseID || chunk.NumSamples() == 0 {
		return PulseMessage{}, false
	}
	c.tracker.Process(chunk.Data, chunk.Timestamps)

	now := chunk.Timestamps[chunk.NumSamples()-1]
	if now < c.nextPulse {
		return PulseMessage{}, false
	}
	c.nextPulse = now + pulseIntervalSec

	snap := c.tracker.Snapshot()
	msg := PulseMessage{
		Type:      "pulse",
		State:     string(snap.State),
		Beats:     snap.Beats,
		BeatCount: snap.BeatCount,
	}
	if snap.Channel >= 0 && snap.Channel < len(c.pulseNames) {
		name := c.pulseNames[snap.Channel]
		msg.Channel = &name
	}
	if snap.HasBPM {
		v := snap.BPM
		msg.BPM = &v
	}
	if snap.HasRMSSD {
		v := snap.RMSSD
		msg.RMSSD = &v
	}
	if snap.HasIBI {
		v := snap.IBIMs
		msg.IBIMs = &v
	}
	return msg, true
}

// readLoop handles control messages until the connection fails, then cancels
// the connection's context so the writer stops too.
func (c *client) readLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn) {
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var ctrl wire.Control
		if err := json.Unmarshal(data, &ctrl); err != nil {
			continue // Ignore junk rather than dropping the viewer.
		}
		if ctrl.Filter != nil {
			c.want.Store(*ctrl.Filter)
		}
		if ctrl.Window != nil {
			c.window.Store(uint64(*ctrl.Window * 1000))
		}
	}
}

// prepare applies this client's filter settings to a chunk, returning either
// the chunk unchanged or a new one. It never writes into c's slices: the same
// chunk is being delivered to every other subscriber.
//
// Only EEG is filtered. A 1-40 Hz bandpass designed for 256 Hz would be
// meaningless on 52 Hz accelerometer data, and the pedestal in the pulse
// streams (classic PPG, Athena optics) is signal, not drift.
func (c *client) prepare(chunk sample.Chunk) sample.Chunk {
	want := c.want.Load()
	if want != c.on {
		if want {
			// Start from a settled filter rather than resuming mid-transient
			// from whenever the viewer last turned it off.
			c.bank.Reset()
			c.dc.Reset()
		}
		c.on = want
	}
	if !c.on || chunk.Stream != sample.EEG {
		return chunk
	}
	if chunk.NumChannels() != c.bank.Channels() {
		return chunk // Not the shape the banks were built for; pass it through.
	}
	return sample.Chunk{
		Stream:     chunk.Stream,
		Timestamps: chunk.Timestamps,
		Data:       c.bank.Process(c.dc.Process(chunk.Data)),
	}
}
