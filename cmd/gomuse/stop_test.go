package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitDone(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled")
	}
}

func TestStopOnStdinStopsOnLineStop(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := stopOnStdin(context.Background(), pr)
	defer cancel()

	io.WriteString(pw, "hello\n")
	select {
	case <-ctx.Done():
		t.Fatal("an unrelated line stopped the recording")
	case <-time.After(50 * time.Millisecond):
	}

	io.WriteString(pw, "  STOP \r\n")
	waitDone(t, ctx)
	if got := stopReason(ctx); got != "stdin-stop" {
		t.Errorf("reason = %q, want stdin-stop", got)
	}
}

func TestStopOnStdinStopsWhenParentCloses(t *testing.T) {
	ctx, cancel := stopOnStdin(context.Background(), strings.NewReader(""))
	defer cancel()
	waitDone(t, ctx)
	if got := stopReason(ctx); got != "stdin-closed" {
		t.Errorf("reason = %q, want stdin-closed", got)
	}
}

func TestStopReason(t *testing.T) {
	live := context.Background()
	if got := stopReason(live); got != "source-ended" {
		t.Errorf("live context: %q", got)
	}
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if got := stopReason(c); got != "interrupt" {
		t.Errorf("cancelled: %q", got)
	}
	d, cancel2 := context.WithTimeoutCause(context.Background(), time.Nanosecond, errDuration)
	defer cancel2()
	<-d.Done()
	if got := stopReason(d); got != "duration" {
		t.Errorf("timeout: %q", got)
	}
}

func TestWriteManifest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "eeg_x.csv")
	if err := os.WriteFile(p, []byte("timestamp,TP9\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := writeManifest(dir, manifest{Tool: "gomuse", StopReason: "duration"}, []string{p}, func(string) int { return 1 })
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files[0].Name != "eeg_x.csv" || m.Files[0].Rows != 1 || m.Files[0].Bytes != 18 {
		t.Fatalf("unexpected files: %+v", m.Files)
	}
	// SHA-256 of "timestamp,TP9\n1,2\n", computed independently.
	if want := "8f662aa70871d8cb7d50d8e5397c78c2c8f2975375aa8b3e0cefc3bbc2d4c157"; m.Files[0].SHA256 != want {
		t.Errorf("sha256 = %s, want %s", m.Files[0].SHA256, want)
	}
	if _, err := os.Stat(out + ".tmp"); err == nil {
		t.Error("temporary manifest file left behind")
	}
}
