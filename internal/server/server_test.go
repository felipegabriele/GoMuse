package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"gomuse/internal/hub"
	"gomuse/internal/sample"
	"gomuse/internal/wire"
)

// readTimeout bounds every blocking read in this file, so a broken server
// fails the test instead of hanging it.
const readTimeout = 5 * time.Second

func newTestServer(t *testing.T) (*Server, *hub.Hub, *httptest.Server) {
	t.Helper()
	h := hub.New()
	s := New(Config{Hub: h})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		h.Close()
	})
	return s, h, ts
}

// dial opens a WebSocket and consumes the hello frame, returning the metadata.
func dial(t *testing.T, ts *httptest.Server) (*websocket.Conn, Hello) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() {
		_ = conn.CloseNow()
	})

	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("reading hello: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("hello message type = %v, want text", typ)
	}
	var h Hello
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatalf("hello is not JSON: %v (%q)", err, data)
	}
	return conn, h
}

// readChunk reads one binary frame and decodes it, failing on timeout.
func readChunk(t *testing.T, conn *websocket.Conn) (sample.Chunk, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()

	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("reading frame: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("frame message type = %v, want binary", typ)
	}
	c, err := wire.DecodeChunk(data)
	if err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	return c, data
}

func sendControl(t *testing.T, conn *websocket.Conn, ctrl wire.Control) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	b, err := json.Marshal(ctrl)
	if err != nil {
		t.Fatalf("marshal control: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write control: %v", err)
	}
}

// constChunk builds an EEG chunk where every sample of every channel is v.
func constChunk(id sample.StreamID, t0, v float64) sample.Chunk {
	m := sample.MetaFor(id)
	c := sample.Chunk{Stream: id, Timestamps: make([]float64, m.SamplesPerPacket)}
	for i := range c.Timestamps {
		c.Timestamps[i] = t0 + float64(i)*m.Period()
	}
	c.Data = make([][]float64, m.NumChannels())
	for ch := range c.Data {
		c.Data[ch] = make([]float64, m.SamplesPerPacket)
		for i := range c.Data[ch] {
			c.Data[ch][i] = v
		}
	}
	return c
}

func get(t *testing.T, ts *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing %s: %v", path, err)
	}
	return resp, body
}

func TestServesAssets(t *testing.T) {
	_, _, ts := newTestServer(t)

	cases := []struct {
		path        string
		contentType string
		contains    string
	}{
		{"/", "text/html", "<"},
		{"/app.js", "javascript", "WebSocket"},
		{"/style.css", "text/css", "{"},
		{"/vendor/uPlot.iife.min.js", "javascript", "uPlot"},
		{"/vendor/uPlot.min.css", "text/css", "{"},
	}
	for _, c := range cases {
		resp, body := get(t, ts, c.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", c.path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, c.contentType) {
			t.Errorf("GET %s: Content-Type %q, want it to contain %q", c.path, ct, c.contentType)
		}
		if len(body) == 0 {
			t.Errorf("GET %s: empty body", c.path)
		}
		if !bytes.Contains(body, []byte(c.contains)) {
			t.Errorf("GET %s: body does not contain %q", c.path, c.contains)
		}
	}
}

func TestIndexIsTheViewer(t *testing.T) {
	_, _, ts := newTestServer(t)
	_, body := get(t, ts, "/")
	for _, want := range []string{"app.js", "style.css", "uPlot"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("index.html does not reference %q", want)
		}
	}
}

func TestHealthz(t *testing.T) {
	_, h, ts := newTestServer(t)

	resp, body := get(t, ts, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type %q, want JSON", ct)
	}
	var hl Health
	if err := json.Unmarshal(body, &hl); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, body)
	}
	if hl.Status != "ok" {
		t.Errorf("status = %q, want %q", hl.Status, "ok")
	}
	if hl.Clients != 0 {
		t.Errorf("clients = %d, want 0", hl.Clients)
	}

	conn, _ := dial(t, ts)
	waitFor(t, "one connected client", func() bool {
		_, b := get(t, ts, "/healthz")
		var h2 Health
		return json.Unmarshal(b, &h2) == nil && h2.Clients == 1
	})
	if err := conn.CloseNow(); err != nil {
		t.Fatalf("close WebSocket: %v", err)
	}
	_ = h
}

func TestHelloDescribesEveryStream(t *testing.T) {
	_, _, ts := newTestServer(t)
	_, hello := dial(t, ts)

	if hello.Type != "hello" {
		t.Errorf("type = %q, want %q", hello.Type, "hello")
	}
	if len(hello.Streams) != len(sample.All()) {
		t.Fatalf("%d streams, want %d", len(hello.Streams), len(sample.All()))
	}
	for i, id := range sample.All() {
		m := sample.MetaFor(id)
		got := hello.Streams[i]
		if got.ID != int(id) || got.Name != m.Name || got.Rate != m.Rate ||
			got.SamplesPerPacket != m.SamplesPerPacket || got.Unit != m.Unit {
			t.Errorf("stream %d = %+v, does not match %+v", i, got, m)
		}
		if strings.Join(got.Channels, ",") != strings.Join(m.Channels, ",") {
			t.Errorf("stream %s channels = %v, want %v", m.Name, got.Channels, m.Channels)
		}
	}
}

func TestForwardsPublishedChunks(t *testing.T) {
	_, h, ts := newTestServer(t)
	conn, _ := dial(t, ts)

	want := sample.Chunk{
		Stream:     sample.EEG,
		Timestamps: []float64{1700000000.0, 1700000000 + 1.0/256, 1700000000 + 2.0/256},
		Data: [][]float64{
			{1, 2, 3}, {-4, -5, -6}, {10, 20, 30}, {0.5, 0.25, 0.125}, {100, 200, 300},
		},
	}
	publishUntilReceived(t, h, want)

	got, _ := readChunkMatching(t, conn, sample.EEG)
	if got.Stream != want.Stream {
		t.Fatalf("stream = %v, want %v", got.Stream, want.Stream)
	}
	if got.NumSamples() != want.NumSamples() || got.NumChannels() != want.NumChannels() {
		t.Fatalf("shape %dx%d, want %dx%d",
			got.NumChannels(), got.NumSamples(), want.NumChannels(), want.NumSamples())
	}
	for ch := range want.Data {
		for i := range want.Data[ch] {
			if math.Abs(got.Data[ch][i]-want.Data[ch][i]) > 1e-4 {
				t.Errorf("ch%d[%d] = %v, want %v", ch, i, got.Data[ch][i], want.Data[ch][i])
			}
		}
	}
	if math.Abs(got.Timestamps[0]-want.Timestamps[0]) > 1e-6 {
		t.Errorf("t0 = %v, want %v", got.Timestamps[0], want.Timestamps[0])
	}
}

// publishUntilReceived publishes c repeatedly until the server's subscriber
// exists. The subscription happens after the handshake, so a single publish
// racing with the connect could be dropped; publishing a few times removes the
// race without making the test timing-dependent.
func publishUntilReceived(t *testing.T, h *hub.Hub, c sample.Chunk) {
	t.Helper()
	for i := 0; i < 20; i++ {
		h.Publish(c)
		time.Sleep(5 * time.Millisecond)
	}
}

// readChunkMatching reads frames until one of the wanted stream arrives.
func readChunkMatching(t *testing.T, conn *websocket.Conn, id sample.StreamID) (sample.Chunk, []byte) {
	t.Helper()
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		c, raw := readChunk(t, conn)
		if c.Stream == id {
			return c, raw
		}
	}
	t.Fatalf("no %s frame within %v", sample.MetaFor(id).Name, readTimeout)
	return sample.Chunk{}, nil
}

func TestFilterTogglesEEGOnlyForTheClientThatAskedFor(t *testing.T) {
	_, h, ts := newTestServer(t)
	conn, _ := dial(t, ts)

	const dc = 500.0
	eeg := constChunk(sample.EEG, 1700000000, dc)
	acc := constChunk(sample.ACC, 1700000000, 0.25)

	// Unfiltered first: the DC offset must arrive intact.
	publishUntilReceived(t, h, eeg)
	got, _ := readChunkMatching(t, conn, sample.EEG)
	if math.Abs(got.Data[0][0]-dc) > 1e-3 {
		t.Fatalf("unfiltered sample = %v, want %v", got.Data[0][0], dc)
	}

	h.Publish(acc)
	_, accUnfiltered := readChunkMatching(t, conn, sample.ACC)

	// Now turn the filter on. The control message is handled asynchronously,
	// so keep publishing until the effect shows up.
	sendControl(t, conn, wire.Control{Filter: boolPtr(true)})

	deadline := time.Now().Add(readTimeout)
	var filtered bool
	for time.Now().Before(deadline) && !filtered {
		h.Publish(eeg)
		c, _ := readChunkMatching(t, conn, sample.EEG)
		if math.Abs(c.Data[0][0]) < 1 {
			filtered = true
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !filtered {
		t.Fatalf("EEG never became filtered: a %v uV constant should filter to ~0", dc)
	}

	// The same accelerometer chunk must still encode identically: filtering
	// is EEG-only.
	h.Publish(acc)
	_, accFiltered := readChunkMatching(t, conn, sample.ACC)
	if !bytes.Equal(accUnfiltered, accFiltered) {
		t.Errorf("ACC frame changed when the filter was enabled:\n%x\n%x", accUnfiltered, accFiltered)
	}

	// And the chunk the hub holds must be untouched, since other subscribers
	// share it.
	for ch := range eeg.Data {
		for i := range eeg.Data[ch] {
			if eeg.Data[ch][i] != dc {
				t.Fatalf("filtering mutated the published chunk at ch%d[%d]: %v", ch, i, eeg.Data[ch][i])
			}
		}
	}
}

// TestFilterIsPerClient: one viewer filtering must not affect another.
func TestFilterIsPerClient(t *testing.T) {
	_, h, ts := newTestServer(t)
	filteredConn, _ := dial(t, ts)
	plainConn, _ := dial(t, ts)

	const dc = 400.0
	eeg := constChunk(sample.EEG, 1700000000, dc)
	sendControl(t, filteredConn, wire.Control{Filter: boolPtr(true)})

	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		h.Publish(eeg)
		a, _ := readChunkMatching(t, filteredConn, sample.EEG)
		b, _ := readChunkMatching(t, plainConn, sample.EEG)
		if math.Abs(b.Data[0][0]-dc) > 1e-3 {
			t.Fatalf("the unfiltered client saw %v, want %v", b.Data[0][0], dc)
		}
		if math.Abs(a.Data[0][0]) < 1 {
			return // filtered client settled, unfiltered client unaffected
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the filtering client never saw filtered data")
}

// TestFilterResetsOnReenable: after a toggle off and on, the filter must start
// settled rather than resuming an old transient.
func TestFilterResetsOnReenable(t *testing.T) {
	_, h, ts := newTestServer(t)
	conn, _ := dial(t, ts)

	big := constChunk(sample.EEG, 1700000000, 900)
	small := constChunk(sample.EEG, 1700000010, 5)

	sendControl(t, conn, wire.Control{Filter: boolPtr(true)})
	settle(t, conn, h, big, func(c sample.Chunk) bool { return math.Abs(c.Data[0][0]) < 1 })

	sendControl(t, conn, wire.Control{Filter: boolPtr(false)})
	settle(t, conn, h, big, func(c sample.Chunk) bool { return math.Abs(c.Data[0][0]-900) < 1e-3 })

	// Re-enable on a completely different level: with a reset, the first
	// filtered sample is ~0 rather than a 900 uV step.
	sendControl(t, conn, wire.Control{Filter: boolPtr(true)})
	settle(t, conn, h, small, func(c sample.Chunk) bool { return math.Abs(c.Data[0][0]) < 1 })
}

func settle(t *testing.T, conn *websocket.Conn, h *hub.Hub, c sample.Chunk, ok func(sample.Chunk) bool) {
	t.Helper()
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		h.Publish(c)
		got, _ := readChunkMatching(t, conn, sample.EEG)
		if ok(got) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never held within the timeout")
}

func TestWindowControlIsAccepted(t *testing.T) {
	_, h, ts := newTestServer(t)
	conn, _ := dial(t, ts)

	w := 7.5
	sendControl(t, conn, wire.Control{Window: &w})
	sendControl(t, conn, wire.Control{}) // an empty control must not break anything

	// The connection must still be delivering frames.
	publishUntilReceived(t, h, constChunk(sample.ACC, 1700000000, 1))
	readChunkMatching(t, conn, sample.ACC)
}

func TestGarbageControlDoesNotKillTheConnection(t *testing.T) {
	_, h, ts := newTestServer(t)
	conn, _ := dial(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
		t.Fatalf("write: %v", err)
	}

	publishUntilReceived(t, h, constChunk(sample.ACC, 1700000000, 1))
	readChunkMatching(t, conn, sample.ACC)
}

func TestDisconnectUnsubscribesCleanly(t *testing.T) {
	s, h, ts := newTestServer(t)

	before := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		conn, _ := dial(t, ts)
		publishUntilReceived(t, h, constChunk(sample.ACC, 1700000000, 1))
		readChunkMatching(t, conn, sample.ACC)
		if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	waitFor(t, "all clients to disconnect", func() bool { return s.Clients() == 0 })

	// With no subscribers left, publishing must neither panic nor drop:
	// a lingering subscription would fill its buffer and count drops.
	dropped := h.Dropped()
	for i := 0; i < 500; i++ {
		h.Publish(constChunk(sample.EEG, 1700000000, float64(i)))
	}
	if got := h.Dropped(); got != dropped {
		t.Errorf("hub dropped %d chunks after every client left, want none", got-dropped)
	}

	waitFor(t, "goroutines to unwind", func() bool { return runtime.NumGoroutine() <= before+2 })
}

func TestPublishAfterServerClosedDoesNotPanic(t *testing.T) {
	s, h, ts := newTestServer(t)
	conn, _ := dial(t, ts)
	publishUntilReceived(t, h, constChunk(sample.ACC, 1700000000, 1))
	readChunkMatching(t, conn, sample.ACC)

	// A hard drop with no close handshake, as a browser tab being killed.
	if err := conn.CloseNow(); err != nil {
		t.Fatalf("close WebSocket: %v", err)
	}
	waitFor(t, "the client to be reaped", func() bool { return s.Clients() == 0 })
	h.Publish(constChunk(sample.EEG, 1700000000, 1))
}

func TestHubCloseEndsTheConnection(t *testing.T) {
	_, h, ts := newTestServer(t)
	conn, _ := dial(t, ts)

	h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return // the server closed the connection, as it should
		}
		if ctx.Err() != nil {
			t.Fatal("connection stayed open after the hub closed")
		}
	}
}

func TestListenAndServeShutsDownOnContextCancel(t *testing.T) {
	h := hub.New()
	defer h.Close()
	s := New(Config{Addr: "127.0.0.1:0", Hub: h})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want nil", err)
		}
	case <-time.After(readTimeout):
		t.Fatal("ListenAndServe did not return after the context was cancelled")
	}
}

func TestNewRequiresAHub(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New with a nil hub did not panic")
		}
	}()
	New(Config{})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func boolPtr(b bool) *bool { return &b }

// newAthenaTestServer builds a server for a headband whose streams are not the
// classic ones, which is the case the device-dependent metadata exists for.
func newAthenaTestServer(t *testing.T) (*hub.Hub, *httptest.Server) {
	t.Helper()
	h := hub.New()
	s := New(Config{Hub: h, Streams: sample.Athena()})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		h.Close()
	})
	return h, ts
}

func TestHelloDescribesTheConnectedDeviceNotTheClassicDefault(t *testing.T) {
	_, ts := newAthenaTestServer(t)
	_, hello := dial(t, ts)

	athena := sample.Athena()
	if len(hello.Streams) != len(athena.All()) {
		t.Fatalf("%d streams, want %d", len(hello.Streams), len(athena.All()))
	}
	for _, s := range hello.Streams {
		if sample.StreamID(s.ID) == sample.PPG {
			t.Error("hello advertises PPG, which an Athena does not have")
		}
		if sample.StreamID(s.ID) != sample.EEG {
			continue
		}
		// The viewer draws one trace per channel named here, so a stale
		// 5-channel default would render a phantom Right AUX trace.
		if len(s.Channels) != 4 {
			t.Errorf("EEG has %d channels, want 4: %v", len(s.Channels), s.Channels)
		}
		for _, name := range s.Channels {
			if name == "Right AUX" {
				t.Error("EEG advertises Right AUX, which an Athena does not have")
			}
		}
	}
}

func TestAthenaEEGChunkSurvivesTheRoundTrip(t *testing.T) {
	h, ts := newAthenaTestServer(t)
	conn, _ := dial(t, ts)

	// A 4-channel chunk would be rejected by a codec still validating against
	// the classic 5-channel layout.
	want := sample.Chunk{
		Stream:     sample.EEG,
		Timestamps: []float64{1000, 1000.00390625},
		Data:       [][]float64{{1, 2}, {3, 4}, {5, 6}, {7, 8}},
	}
	go publishUntilReceived(t, h, want)

	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("reading frame: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("frame type = %v, want binary", typ)
	}

	got, err := wire.NewCodec(sample.Athena()).DecodeChunk(data)
	if err != nil {
		t.Fatalf("decoding frame: %v", err)
	}
	if got.NumChannels() != 4 {
		t.Errorf("got %d channels, want 4", got.NumChannels())
	}
	if got.Stream != sample.EEG {
		t.Errorf("stream = %d, want EEG", got.Stream)
	}
}
