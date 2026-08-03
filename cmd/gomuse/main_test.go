package main

import (
	"context"
	"strings"
	"testing"

	"gomuse/internal/hub"
)

// Demo mode is served by internal/synth, which has no optics waveform. These
// tests pin the guard that turns that gap into an immediate error instead of a
// run that publishes nothing: -optics on its own passes connFlags.any (it is
// exactly how a real Athena is driven), so nothing else would catch it.
//
// Both cases are hardware-free and finish instantly: start's demo branch only
// constructs the generators, it never runs them.

func TestStartDemoRejectsOpticsOnly(t *testing.T) {
	h := hub.New()
	defer h.Close()

	cf := connFlags{demo: true, optics: true}
	src, err := cf.start(context.Background(), h)
	if err == nil {
		src.stop()
		t.Fatal("optics-only demo config was accepted; it can never produce a sample")
	}
	// The message has to say what went wrong, not just that nothing is on:
	// the user did enable a stream, it is simply one demo cannot synthesise.
	if !strings.Contains(err.Error(), "optics") {
		t.Errorf("error does not mention optics: %v", err)
	}
	for _, want := range []string{"eeg", "ppg", "acc", "gyro"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not offer %q as an alternative: %v", want, err)
		}
	}
}

func TestStartDemoAcceptsSynthesizableStream(t *testing.T) {
	h := hub.New()
	defer h.Close()

	// Optics stays enabled, as it is by default: the guard must reject only
	// the case where *nothing* enabled can be generated, not any mention of
	// optics, or the default `gomuse stream -demo` would stop working.
	cf := connFlags{demo: true, optics: true, eeg: true}
	src, err := cf.start(context.Background(), h)
	if err != nil {
		t.Fatalf("demo config with EEG enabled was rejected: %v", err)
	}
	defer src.stop()
	if src.streams == nil {
		t.Fatal("demo source reported no stream set")
	}
}
