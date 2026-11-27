package main

import (
	"encoding/json"
	"io"
	"time"
)

// statusEvent is one line of machine-readable progress, written to standard
// output with -ready-json. A parent process reads these instead of scraping the
// human messages on standard error, which are free to change.
type statusEvent struct {
	// Event is "recording" once data is being written, or "finished" when the
	// recording has ended.
	Event string `json:"event"`
	// UnixUS is the host wall clock, in microseconds since the Unix epoch, at
	// the moment the event was written. It is the same clock as the CSV
	// timestamps.
	UnixUS int64  `json:"unix_us"`
	Dir    string `json:"dir"`
	Demo   bool   `json:"demo"`
	// StopReason and Manifest are set on "finished" only. Manifest is empty when
	// -manifest was not given.
	StopReason string `json:"stop_reason,omitempty"`
	Manifest   string `json:"manifest,omitempty"`
}

// emitStatus writes one event as a single JSON line. Errors are ignored: a
// parent that closed the pipe must not turn a good recording into a failure.
func emitStatus(w io.Writer, ev statusEvent) {
	if w == nil {
		return
	}
	if ev.UnixUS == 0 {
		ev.UnixUS = time.Now().UnixMicro()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}
