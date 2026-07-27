package muse

import (
	"strings"
	"testing"

	"gomuse/internal/sample"
)

// classic is the characteristic set of a Muse 2 / Muse S (pre-Athena).
func classic() map[string]bool {
	present := map[string]bool{CharControl: true, CharTelemetry: true}
	for _, u := range EEGChannelUUIDs {
		present[u] = true
	}
	for _, u := range PPGChannelUUIDs {
		present[u] = true
	}
	present[CharAccelerometer] = true
	present[CharGyro] = true
	return present
}

// athena is what a Muse S Athena (firmware 3.1.23) actually exposes: control
// plus three multiplexed characteristics, and nothing else.
func athena() map[string]bool {
	present := map[string]bool{CharControl: true}
	for _, u := range AthenaCharUUIDs {
		present[u] = true
	}
	return present
}

func without(present map[string]bool, uuids ...string) map[string]bool {
	out := map[string]bool{}
	for k, v := range present {
		out[k] = v
	}
	for _, u := range uuids {
		delete(out, u)
	}
	return out
}

func allStreams() Config {
	return Config{Streams: map[sample.StreamID]bool{
		sample.EEG: true, sample.PPG: true, sample.ACC: true, sample.Gyro: true,
	}}
}

func TestMissingCharsClassicHeadbandHasEverything(t *testing.T) {
	if got := missingChars(classic(), allStreams()); len(got) != 0 {
		t.Fatalf("classic headband reported missing characteristics: %v", got)
	}
}

func TestMissingCharsOnlyCoversRequestedStreams(t *testing.T) {
	// A headband with no PPG is complete as far as an EEG-only config cares.
	noPPG := without(classic(), PPGChannelUUIDs...)
	cfg := Config{Streams: map[sample.StreamID]bool{sample.EEG: true}}
	if got := missingChars(noPPG, cfg); len(got) != 0 {
		t.Fatalf("EEG-only config reported missing: %v", got)
	}

	got := missingChars(noPPG, allStreams())
	want := PPGChannelUUIDs
	if len(got) != len(want) {
		t.Fatalf("missingChars = %v, want the %d PPG characteristics", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("missingChars[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestIsAthena(t *testing.T) {
	if !isAthena(athena()) {
		t.Error("isAthena(athena) = false, want true")
	}
	if isAthena(classic()) {
		t.Error("isAthena(classic) = true, want false")
	}

	// A firmware exposing both layouts is a classic headband as far as we are
	// concerned: the per-channel characteristics are what we drive.
	both := classic()
	for _, u := range AthenaCharUUIDs {
		both[u] = true
	}
	if isAthena(both) {
		t.Error("isAthena(classic+athena chars) = true, want false")
	}
}

// An Athena is routed to setupAthena before the classic layout is validated,
// so unsupportedDevice should never see one. If a future refactor reorders
// that branch, the error must say so rather than listing ten missing UUIDs and
// sending the reader hunting for absent hardware.
func TestUnsupportedDeviceOnAthenaReportsARoutingBug(t *testing.T) {
	present := athena()
	missing := missingChars(present, allStreams())
	if len(missing) == 0 {
		t.Fatal("athena layout reported no missing characteristics")
	}

	msg := unsupportedDevice(present, missing).Error()
	if !strings.Contains(msg, "Athena") {
		t.Errorf("error does not name Athena: %s", msg)
	}
	if !strings.Contains(msg, "bug") {
		t.Errorf("error does not flag itself as a routing bug: %s", msg)
	}
	if strings.Contains(msg, CharTP9) {
		t.Errorf("Athena error lists classic UUIDs: %s", msg)
	}
}

func TestUnsupportedDeviceNamesMissingCharacteristicsAndHint(t *testing.T) {
	present := without(classic(), PPGChannelUUIDs...)
	missing := missingChars(present, allStreams())

	err := unsupportedDevice(present, missing)
	msg := err.Error()
	for _, want := range []string{"PPG ambient", "PPG infrared", "PPG red", "-ppg=false"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "Athena") {
		t.Errorf("classic headband misreported as Athena: %v", err)
	}
}

func TestDisableHintCoversEveryStream(t *testing.T) {
	present := map[string]bool{CharControl: true}
	missing := missingChars(present, allStreams())

	hint := disableHint(missing)
	for _, want := range []string{"-eeg=false", "-ppg=false", "-acc=false", "-gyro=false"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q does not mention %q", hint, want)
		}
	}
}

func TestDisableHintEmptyWhenOnlyControlIsMissing(t *testing.T) {
	// No flag combination recovers a headband with no control characteristic,
	// so the error should not pretend otherwise.
	present := without(classic(), CharControl)
	missing := missingChars(present, allStreams())

	if got := disableHint(missing); got != "" {
		t.Errorf("disableHint = %q, want empty", got)
	}
	if msg := unsupportedDevice(present, missing).Error(); !strings.Contains(msg, "control") {
		t.Errorf("error %q does not mention the control characteristic", msg)
	}
}
