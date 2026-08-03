package muse

import (
	"encoding/binary"
	"sync"
)

// Telemetry is the classic headband's housekeeping packet: battery, fuel gauge,
// ADC voltage and temperature, arriving on CharTelemetry roughly every ten
// seconds. Only the battery is surfaced to the viewer; the rest is decoded
// because the packet has to be walked past it anyway, and because muse-lsl
// carries all four.
type Telemetry struct {
	// Index is the packet counter, the same 16-bit sequence number the data
	// characteristics carry. Unused here — telemetry is not a sample stream, so
	// there is nothing to dejitter or reassemble against.
	Index uint16

	BatteryPct  float64
	FuelGauge   float64
	ADCVolt     float64
	Temperature float64
}

const (
	// TelemetryBatteryScale converts the raw uint16 to percent. The Athena uses
	// the same 1/512, on a differently framed packet — see AthenaBatteryScale.
	TelemetryBatteryScale = 1.0 / 512.0

	// TelemetryFuelGaugeScale is muse-lsl's factor for the fuel gauge word.
	TelemetryFuelGaugeScale = 2.2

	// telemetryWords is how many uint16s the packet carries before its zero
	// padding: index, battery, fuel gauge, ADC voltage, temperature.
	telemetryWords = 5
)

// DecodeTelemetry parses a classic telemetry packet. It is a port of muse-lsl's
// Muse._handle_telemetry, and the scale factors are its.
//
// The words are big-endian, like everything else on the classic path (muse-lsl
// reads them as "uint:16,uint:16,uint:16,uint:16,uint:16" through bitstring,
// which is MSB-first — the same order as the bit-packed EEG and PPG samples in
// decode.go). This is the opposite of the Athena's battery block, which is
// little-endian; see decodeAthenaBattery.
//
// A short packet is rejected rather than zero-filled: a truncated notification
// would otherwise report a plausible-looking 0% battery.
func DecodeTelemetry(data []byte) (Telemetry, bool) {
	if len(data) < telemetryWords*2 {
		return Telemetry{}, false
	}
	var w [telemetryWords]uint16
	for i := range w {
		w[i] = binary.BigEndian.Uint16(data[i*2 : i*2+2])
	}
	return Telemetry{
		Index:       w[0],
		BatteryPct:  float64(w[1]) * TelemetryBatteryScale,
		FuelGauge:   float64(w[2]) * TelemetryFuelGaugeScale,
		ADCVolt:     float64(w[3]),
		Temperature: float64(w[4]),
	}, true
}

// batteryState is the last battery reading from whichever protocol reported it.
//
// The "reported at all" flag is not redundant with a zero percentage: telemetry
// is slow (about one packet every ten seconds on a classic headband, one every
// five on an Athena), so there is a real window after connecting during which
// the level is genuinely unknown, and the viewer must show that rather than
// invent a number.
type batteryState struct {
	mu  sync.Mutex
	pct float64
	ok  bool
}

func (b *batteryState) set(pct float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pct, b.ok = pct, true
}

func (b *batteryState) get() (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pct, b.ok
}
