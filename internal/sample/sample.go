// Package sample defines the data types shared by every layer of gomuse: the
// BLE decoder that produces samples, the hub that fans them out, the WebSocket
// encoder, and the CSV recorder.
//
// It deliberately has no dependencies, so that none of those layers need to
// import the Bluetooth stack.
package sample

// StreamID identifies a sensor stream. The numeric values are part of the
// WebSocket wire format and must not be reordered.
type StreamID uint8

const (
	EEG  StreamID = 0
	PPG  StreamID = 1
	ACC  StreamID = 2
	Gyro StreamID = 3
)

// Meta describes the fixed properties of a stream.
type Meta struct {
	ID StreamID
	// Name is the lowercase identifier used in CSV filenames and the UI.
	Name string
	// Channels are the per-channel labels, in the order they appear in a
	// Chunk's Data slice.
	Channels []string
	// Unit is the physical unit of the samples.
	Unit string
	// Rate is the nominal sampling rate in Hz.
	Rate float64
	// SamplesPerPacket is how many samples per channel arrive in one BLE
	// notification.
	SamplesPerPacket int
}

// NumChannels returns the channel count for the stream.
func (m Meta) NumChannels() int { return len(m.Channels) }

// Period returns the nominal seconds between consecutive samples.
func (m Meta) Period() float64 { return 1 / m.Rate }

var metas = map[StreamID]Meta{
	EEG: {
		ID:               EEG,
		Name:             "eeg",
		Channels:         []string{"TP9", "AF7", "AF8", "TP10", "Right AUX"},
		Unit:             "uV",
		Rate:             256,
		SamplesPerPacket: 12,
	},
	PPG: {
		ID:               PPG,
		Name:             "ppg",
		Channels:         []string{"PPG1", "PPG2", "PPG3"},
		Unit:             "",
		Rate:             64,
		SamplesPerPacket: 6,
	},
	ACC: {
		ID:               ACC,
		Name:             "acc",
		Channels:         []string{"X", "Y", "Z"},
		Unit:             "g",
		Rate:             52,
		SamplesPerPacket: 3,
	},
	Gyro: {
		ID:               Gyro,
		Name:             "gyro",
		Channels:         []string{"X", "Y", "Z"},
		Unit:             "dps",
		Rate:             52,
		SamplesPerPacket: 3,
	},
}

// MetaFor returns the metadata for a stream. It panics on an unknown ID, which
// can only happen if a caller invents one: the set is closed.
func MetaFor(id StreamID) Meta {
	m, ok := metas[id]
	if !ok {
		panic("sample: unknown stream id")
	}
	return m
}

// All returns every stream ID in wire order.
func All() []StreamID { return []StreamID{EEG, PPG, ACC, Gyro} }

// Chunk is one decoded BLE notification: a short block of samples for every
// channel of a single stream.
//
// Data is channel-major: Data[c][i] is sample i of channel c. Every inner
// slice has the same length, and Timestamps has that length too — Timestamps[i]
// is the instant of sample i across all channels.
type Chunk struct {
	Stream     StreamID
	Timestamps []float64 // Unix seconds
	Data       [][]float64
}

// NumSamples returns the samples per channel in the chunk.
func (c Chunk) NumSamples() int { return len(c.Timestamps) }

// NumChannels returns the channel count of the chunk.
func (c Chunk) NumChannels() int { return len(c.Data) }
