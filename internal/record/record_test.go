package record

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gomuse/internal/hub"
	"gomuse/internal/sample"
)

// fixedNow is the injected clock every test uses, so filenames are predictable.
var fixedNow = time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)

const fixedStamp = "2024-03-04_05-06-07"

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Dir: t.TempDir(),
		Now: func() time.Time { return fixedNow },
	}
}

// chunk builds a well formed chunk for id with n samples. Sample i of channel c
// is c*100+i so every cell is identifiable, and timestamps start at base.
func chunk(id sample.StreamID, base float64, n int) sample.Chunk {
	m := sample.MetaFor(id)
	c := sample.Chunk{Stream: id}
	for i := 0; i < n; i++ {
		c.Timestamps = append(c.Timestamps, base+float64(i)*m.Period())
	}
	for ch := 0; ch < m.NumChannels(); ch++ {
		col := make([]float64, n)
		for i := range col {
			col[i] = float64(ch*100 + i)
		}
		c.Data = append(c.Data, col)
	}
	return c
}

// runRecorder starts r against h and returns a stop func that cancels the run
// and waits for it to finish, failing the test if it does not.
func runRecorder(t *testing.T, r *Recorder, h *hub.Hub) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, h) }()

	// Run must have subscribed before the test publishes, or chunks vanish.
	waitFor(t, r.subscribed)

	var once bool
	return func() {
		t.Helper()
		if once {
			return
		}
		once = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for Run to return")
		}
	}
}

// waitFor blocks until ch is signalled or the test times out.
func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recorder")
	}
}

// readCSV parses a file written by the recorder.
func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	return recs
}

func pathFor(dir string, id sample.StreamID) string {
	return filepath.Join(dir, sample.MetaFor(id).Name+"_"+fixedStamp+".csv")
}

func TestHeadersComeFromMetadata(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()
	stop := runRecorder(t, r, h)

	for _, id := range sample.All() {
		h.Publish(chunk(id, 1700000000, 2))
	}
	stop()

	for _, id := range sample.All() {
		m := sample.MetaFor(id)
		recs := readCSV(t, pathFor(cfg.Dir, id))
		want := append([]string{"timestamp"}, m.Channels...)
		if len(recs) == 0 {
			t.Fatalf("%s: no records", m.Name)
		}
		if strings.Join(recs[0], ",") != strings.Join(want, ",") {
			t.Errorf("%s header = %v, want %v", m.Name, recs[0], want)
		}
	}
}

func TestOneRowPerSampleNotPerChunk(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()
	stop := runRecorder(t, r, h)

	h.Publish(chunk(sample.EEG, 1700000000, 12))
	h.Publish(chunk(sample.EEG, 1700000001, 12))
	stop()

	recs := readCSV(t, pathFor(cfg.Dir, sample.EEG))
	if got, want := len(recs), 1+24; got != want {
		t.Fatalf("got %d lines, want %d (header + 24 samples)", got, want)
	}
	if got := r.Rows()[sample.EEG]; got != 24 {
		t.Errorf("Rows()[EEG] = %d, want 24", got)
	}

	// Row 3 of the first chunk: sample index 2 of every channel.
	row := recs[3]
	m := sample.MetaFor(sample.EEG)
	if len(row) != 1+m.NumChannels() {
		t.Fatalf("row = %v, want %d fields", row, 1+m.NumChannels())
	}
	for ch := 0; ch < m.NumChannels(); ch++ {
		want := strconv.Itoa(ch*100 + 2)
		if row[1+ch] != want {
			t.Errorf("row[%d] = %q, want %q", 1+ch, row[1+ch], want)
		}
	}
}

func TestTimestampsKeepMicrosecondPrecision(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()
	stop := runRecorder(t, r, h)

	// A realistic Unix time with sub-millisecond detail. Default float
	// formatting would render this as 1.700000000123456e+09.
	const ts = 1700000000.123456
	h.Publish(sample.Chunk{
		Stream:     sample.ACC,
		Timestamps: []float64{ts},
		Data:       [][]float64{{1}, {2}, {3}},
	})
	stop()

	recs := readCSV(t, pathFor(cfg.Dir, sample.ACC))
	if len(recs) != 2 {
		t.Fatalf("got %d lines, want header + 1 row", len(recs))
	}
	want := strconv.FormatFloat(ts, 'f', 6, 64)
	if want != "1700000000.123456" {
		t.Fatalf("test premise wrong: %q", want)
	}
	if recs[1][0] != want {
		t.Errorf("timestamp = %q, want %q", recs[1][0], want)
	}
	if strings.ContainsAny(recs[1][0], "eE") {
		t.Errorf("timestamp %q uses exponent notation", recs[1][0])
	}
}

func TestFilesAreCreatedLazily(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()
	stop := runRecorder(t, r, h)

	// Only EEG ever arrives, as with a preset that carries no PPG.
	h.Publish(chunk(sample.EEG, 1700000000, 12))
	stop()

	entries, err := os.ReadDir(cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(pathFor(cfg.Dir, sample.EEG)) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory contains %v, want only the eeg file", names)
	}
	if got := r.Files(); len(got) != 1 || got[0] != pathFor(cfg.Dir, sample.EEG) {
		t.Errorf("Files() = %v, want just the eeg file", got)
	}
	if _, ok := r.Rows()[sample.PPG]; ok {
		t.Errorf("Rows() reports PPG though none arrived: %v", r.Rows())
	}
}

func TestNoFilesAtAllWhenNothingArrives(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()
	runRecorder(t, r, h)()

	entries, err := os.ReadDir(cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d files, want none", len(entries))
	}
	if got := r.Files(); len(got) != 0 {
		t.Errorf("Files() = %v, want none", got)
	}
}

func TestCancelMidRunLeavesValidCSV(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, h) }()
	waitFor(t, r.subscribed)

	for i := 0; i < 20; i++ {
		h.Publish(chunk(sample.EEG, 1700000000+float64(i), 12))
	}
	cancel() // Ctrl-C in the middle of a recording.

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	recs := readCSV(t, pathFor(cfg.Dir, sample.EEG))
	if len(recs) != 1+20*12 {
		t.Fatalf("got %d lines, want %d: buffered chunks were lost", len(recs), 1+20*12)
	}
	for i, rec := range recs {
		if len(rec) != 6 {
			t.Fatalf("line %d is ragged: %v", i, rec)
		}
	}
	// The last row must be complete, not a partial write.
	last := recs[len(recs)-1]
	if _, err := strconv.ParseFloat(last[0], 64); err != nil {
		t.Errorf("last timestamp %q not parseable: %v", last[0], err)
	}
}

func TestRunReturnsWhenHubCloses(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), h) }()
	waitFor(t, r.subscribed)

	h.Publish(chunk(sample.Gyro, 1700000000, 3))
	h.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after hub close")
	}
	if got := r.Rows()[sample.Gyro]; got != 3 {
		t.Errorf("Rows()[Gyro] = %d, want 3", got)
	}
}

func TestStreamFilter(t *testing.T) {
	cfg := testConfig(t)
	cfg.Streams = map[sample.StreamID]bool{sample.ACC: true}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()
	stop := runRecorder(t, r, h)

	h.Publish(chunk(sample.EEG, 1700000000, 12))
	h.Publish(chunk(sample.ACC, 1700000000, 3))
	stop()

	if got := r.Files(); len(got) != 1 || got[0] != pathFor(cfg.Dir, sample.ACC) {
		t.Fatalf("Files() = %v, want only the acc file", got)
	}
	if got := r.Rows()[sample.ACC]; got != 3 {
		t.Errorf("Rows()[ACC] = %d, want 3", got)
	}
}

func TestMalformedChunksAreSkippedAndCounted(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()
	stop := runRecorder(t, r, h)

	// Too few channels for the stream's metadata.
	h.Publish(sample.Chunk{
		Stream:     sample.EEG,
		Timestamps: []float64{1, 2},
		Data:       [][]float64{{1, 2}, {3, 4}},
	})
	// Right channel count, but a column shorter than Timestamps.
	h.Publish(sample.Chunk{
		Stream:     sample.EEG,
		Timestamps: []float64{1, 2, 3},
		Data:       [][]float64{{1, 2, 3}, {1, 2, 3}, {1, 2}, {1, 2, 3}, {1, 2, 3}},
	})
	// PPG only ever sends malformed chunks: no file should appear.
	h.Publish(sample.Chunk{
		Stream:     sample.PPG,
		Timestamps: []float64{1},
		Data:       nil,
	})
	// A good chunk still gets through afterwards.
	h.Publish(chunk(sample.EEG, 1700000000, 12))
	stop()

	if got := r.Malformed(); got != 3 {
		t.Errorf("Malformed() = %d, want 3", got)
	}
	if _, err := os.Stat(pathFor(cfg.Dir, sample.PPG)); !os.IsNotExist(err) {
		t.Errorf("ppg file exists though every ppg chunk was malformed (err=%v)", err)
	}
	recs := readCSV(t, pathFor(cfg.Dir, sample.EEG))
	if len(recs) != 1+12 {
		t.Fatalf("got %d lines, want header + 12 good samples", len(recs))
	}
	if got := r.Rows()[sample.EEG]; got != 12 {
		t.Errorf("Rows()[EEG] = %d, want 12", got)
	}
}

func TestSubscribesWithLargeBuffer(t *testing.T) {
	if DefaultBuffer < 1024 {
		t.Errorf("DefaultBuffer = %d, too small to survive a disk stall", DefaultBuffer)
	}
}

func TestDropsAreReported(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A one-chunk buffer plus a writer parked on a gate makes the drop
	// deterministic instead of timing dependent.
	r.buffer = 1
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	r.onChunk = func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
	}

	h := hub.New()
	defer h.Close()
	stop := runRecorder(t, r, h)

	h.Publish(chunk(sample.ACC, 1700000000, 3)) // taken by the writer, blocks
	waitFor(t, entered)
	h.Publish(chunk(sample.ACC, 1700000001, 3)) // fills the buffer
	h.Publish(chunk(sample.ACC, 1700000002, 3)) // dropped
	close(gate)
	stop()

	if h.Dropped() == 0 {
		t.Fatal("test premise wrong: hub dropped nothing")
	}
	if got := r.Dropped(); got == 0 {
		t.Errorf("Dropped() = 0, want the run's drops surfaced (hub saw %d)", h.Dropped())
	}
}

func TestDropsIgnorePreExistingCount(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	defer h.Close()

	// Another subscriber drops chunks before the recorder ever starts.
	other, cancelOther := h.Subscribe(0)
	_ = other
	for i := 0; i < 5; i++ {
		h.Publish(chunk(sample.ACC, float64(i), 3))
	}
	cancelOther()
	if h.Dropped() == 0 {
		t.Fatal("test premise wrong: expected pre-run drops")
	}

	stop := runRecorder(t, r, h)
	h.Publish(chunk(sample.ACC, 1700000000, 3))
	stop()

	if got := r.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0: drops from before the run leaked in", got)
	}
}

func TestNewCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "out")
	r, err := New(Config{Dir: dir, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if r == nil {
		t.Fatal("New returned nil recorder")
	}
}
