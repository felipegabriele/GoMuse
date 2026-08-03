package muse

import (
	"testing"

	"gomuse/internal/sample"
)

// telemetryPacket builds a classic telemetry notification from its five words.
func telemetryPacket(words ...uint16) []byte {
	out := make([]byte, 0, len(words)*2)
	for _, w := range words {
		out = append(out, byte(w>>8), byte(w))
	}
	return out
}

func TestDecodeTelemetry(t *testing.T) {
	// 0x9C00 = 39936, and 39936/512 = 78%.
	packet := telemetryPacket(0x0007, 0x9C00, 0x0064, 0x0FA0, 0x0015)

	got, ok := DecodeTelemetry(packet)
	if !ok {
		t.Fatal("DecodeTelemetry rejected a well-formed packet")
	}
	if got.Index != 7 {
		t.Errorf("Index = %d, want 7", got.Index)
	}
	if !closeEnough(got.BatteryPct, 78, 1e-9) {
		t.Errorf("BatteryPct = %f, want 78", got.BatteryPct)
	}
	if !closeEnough(got.FuelGauge, 100*2.2, 1e-9) {
		t.Errorf("FuelGauge = %f, want %f", got.FuelGauge, 100*2.2)
	}
	if !closeEnough(got.ADCVolt, 4000, 1e-9) {
		t.Errorf("ADCVolt = %f, want 4000", got.ADCVolt)
	}
	if !closeEnough(got.Temperature, 21, 1e-9) {
		t.Errorf("Temperature = %f, want 21", got.Temperature)
	}
}

// The words are big-endian, unlike the Athena's little-endian battery block.
// Getting this backwards still decodes without error, so it needs its own test
// rather than being implied by the one above: 0x9C00 read the wrong way round
// is 0x009C, which is a plausible-looking 0.3% rather than an obvious failure.
func TestDecodeTelemetryIsBigEndian(t *testing.T) {
	got, ok := DecodeTelemetry(telemetryPacket(0, 0x0100, 0, 0, 0))
	if !ok {
		t.Fatal("DecodeTelemetry rejected a well-formed packet")
	}
	if !closeEnough(got.BatteryPct, 0.5, 1e-9) {
		t.Errorf("BatteryPct = %f, want 0.5 (0x0100/512); "+
			"a little-endian read would give %f", got.BatteryPct, 1.0/512.0*1)
	}
}

func TestDecodeTelemetryFullScale(t *testing.T) {
	// 51200/512 = 100%, the level a fully charged headband reports.
	got, ok := DecodeTelemetry(telemetryPacket(0, 51200, 0, 0, 0))
	if !ok {
		t.Fatal("DecodeTelemetry rejected a well-formed packet")
	}
	if !closeEnough(got.BatteryPct, 100, 1e-9) {
		t.Errorf("BatteryPct = %f, want 100", got.BatteryPct)
	}
}

// A truncated packet must be rejected rather than zero-filled: the whole point
// of the ok return is that a short read cannot be reported as 0% battery.
func TestDecodeTelemetryRejectsShortPackets(t *testing.T) {
	full := telemetryPacket(0, 0x9C00, 0, 0, 0)
	for n := 0; n < len(full); n++ {
		if _, ok := DecodeTelemetry(full[:n]); ok {
			t.Errorf("DecodeTelemetry accepted a %d-byte packet", n)
		}
	}
	if _, ok := DecodeTelemetry(full); !ok {
		t.Error("DecodeTelemetry rejected the full-length packet")
	}
}

// Trailing zero padding is normal — the real notification is 20 bytes — and
// must not confuse the decode of the words in front of it.
func TestDecodeTelemetryIgnoresPadding(t *testing.T) {
	packet := append(telemetryPacket(1, 0x9C00, 0, 0, 0), make([]byte, 10)...)
	got, ok := DecodeTelemetry(packet)
	if !ok {
		t.Fatal("DecodeTelemetry rejected a padded packet")
	}
	if !closeEnough(got.BatteryPct, 78, 1e-9) {
		t.Errorf("BatteryPct = %f, want 78", got.BatteryPct)
	}
}

func TestBatteryStateReportsUnknownUntilSet(t *testing.T) {
	var b batteryState
	if _, ok := b.get(); ok {
		t.Error("a fresh batteryState reports a known level")
	}

	b.set(42)
	got, ok := b.get()
	if !ok {
		t.Fatal("batteryState reports unknown after set")
	}
	if !closeEnough(got, 42, 1e-9) {
		t.Errorf("battery = %f, want 42", got)
	}

	// Zero is a real level, not a signal that nothing has been reported.
	b.set(0)
	got, ok = b.get()
	if !ok || got != 0 {
		t.Errorf("battery after set(0) = (%f, %v), want (0, true)", got, ok)
	}
}

// Client.Battery must route to whichever protocol is in play, and report
// unknown on a classic client that has had no telemetry yet.
func TestClientBatteryUnknownBeforeAnyTelemetry(t *testing.T) {
	var c Client
	if pct, ok := c.Battery(); ok {
		t.Errorf("Battery() = (%f, %v) before any telemetry, want unknown", pct, ok)
	}

	c.battery.set(63)
	pct, ok := c.Battery()
	if !ok || !closeEnough(pct, 63, 1e-9) {
		t.Errorf("Battery() = (%f, %v), want (63, true)", pct, ok)
	}
}

func TestClientBatteryPrefersTheAthenaStreamer(t *testing.T) {
	c := Client{athena: newAthenaStreamer(Config{}, func(sample.Chunk) {}, func() {}, fakeClock(1000, 0.01))}
	// The classic field is set, but an Athena client must read its own
	// streamer: the classic telemetry characteristic does not exist there, so a
	// value in this field could only be stale or wrong.
	c.battery.set(63)
	if pct, ok := c.Battery(); ok {
		t.Errorf("Battery() = (%f, %v) on an Athena with no battery block, want unknown", pct, ok)
	}
}
