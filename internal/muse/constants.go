// Package muse implements BLE communication with Muse brain-sensing headbands
// and decoding of their data packets.
//
// This is a Go port of the device layer of muse-lsl
// (https://github.com/alexandrebarachant/muse-lsl).
package muse

// GATT UUIDs. The Interaxon service is 0000fe8d-0000-1000-8000-00805f9b34fb;
// every data and control characteristic lives under the 273e00xx vendor range.
const (
	ServiceUUID = "0000fe8d-0000-1000-8000-00805f9b34fb"

	// CharControl accepts command strings and emits status replies.
	CharControl = "273e0001-4c4d-454d-96be-f03bac821358"

	// EEG channels, in canonical order.
	CharTP9      = "273e0003-4c4d-454d-96be-f03bac821358"
	CharAF7      = "273e0004-4c4d-454d-96be-f03bac821358"
	CharAF8      = "273e0005-4c4d-454d-96be-f03bac821358"
	CharTP10     = "273e0006-4c4d-454d-96be-f03bac821358"
	CharRightAUX = "273e0007-4c4d-454d-96be-f03bac821358"

	CharGyro          = "273e0009-4c4d-454d-96be-f03bac821358"
	CharAccelerometer = "273e000a-4c4d-454d-96be-f03bac821358"
	CharTelemetry     = "273e000b-4c4d-454d-96be-f03bac821358"

	// PPG channels: ambient, infrared, red.
	CharPPG1 = "273e000f-4c4d-454d-96be-f03bac821358"
	CharPPG2 = "273e0010-4c4d-454d-96be-f03bac821358"
	CharPPG3 = "273e0011-4c4d-454d-96be-f03bac821358"
)

// EEGChannelUUIDs lists the EEG characteristics in channel order, so that the
// index into this slice is the index into a Chunk's channel dimension.
//
// muse-lsl derives this index from the GATT handle (index = (handle-32)/3).
// CoreBluetooth does not expose handles, so we key off the UUID instead.
var EEGChannelUUIDs = []string{CharTP9, CharAF7, CharAF8, CharTP10, CharRightAUX}

// PPGChannelUUIDs lists the PPG characteristics in channel order.
var PPGChannelUUIDs = []string{CharPPG1, CharPPG2, CharPPG3}

// Sampling rates, channel counts and samples-per-packet are properties of the
// stream rather than of the BLE layer; they live in package sample so that the
// hub, recorder and server can reach them without importing Bluetooth. Use
// sample.MetaFor(sample.EEG) and friends.

// Conversion factors from raw counts to physical units.
const (
	// EEGScaleFactor converts a 12-bit count to microvolts over a 2 mVpp range.
	// Raw values are centred on EEGZeroOffset.
	EEGScaleFactor = 0.48828125
	EEGZeroOffset  = 2048

	// AccelerometerScaleFactor converts a signed 16-bit count to g.
	AccelerometerScaleFactor = 0.0000610352

	// GyroScaleFactor converts a signed 16-bit count to degrees per second.
	GyroScaleFactor = 0.0074768
)

// PacketSize is the byte length of every EEG, PPG and IMU notification:
// a 16-bit packet index followed by the bit-packed samples.
const PacketSize = 20

// Timing behaviour, mirroring muse-lsl's constants.py.
const (
	// ScanTimeoutSeconds bounds a device scan.
	ScanTimeoutSeconds = 10.5
	// AutoDisconnectSeconds is how long to wait after the device stops sending
	// data before ending the stream.
	AutoDisconnectSeconds = 3
	// KeepAliveSeconds is the interval between 'k' commands.
	KeepAliveSeconds = 10
)

// Presets. muse-lsl defaults to p21, which streams EEG only. PPG requires one
// of the p5x presets on Muse 2 / Muse S; p50 is the starting candidate and
// p51..p53 are the fallbacks if the PPG characteristics never notify.
const (
	PresetEEGOnly = "p21"
	PresetWithPPG = "p50"
)
