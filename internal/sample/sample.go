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
	// Optics is the Muse S Athena's 16-channel photoplethysmography stream.
	// It is the Athena's equivalent of PPG rather than a second copy of it:
	// that headband has no PPG characteristic, so no device produces both.
	// Appended rather than slotted next to PPG because the numeric values are
	// the WebSocket wire format.
	Optics StreamID = 4
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

// Set is the stream metadata of one connected device.
//
// The shape of the data is a property of the device, not a global constant: a
// classic Muse 2 / Muse S has 5 EEG channels and 3 PPG channels, while a Muse S
// Athena has 4 EEG channels and no PPG at all. Every layer that needs to know
// how wide a stream is — the wire encoder, the recorder's CSV header, the
// viewer's hello message — takes a Set rather than consulting a package global,
// so that connecting a different headband cannot silently produce frames whose
// channel count disagrees with what the browser was told.
//
// A Set is immutable once built and safe for concurrent use.
type Set struct {
	metas map[StreamID]Meta
	order []StreamID
}

// NewSet builds a Set from the given metadata, preserving wire order: the
// caller lists streams in ascending StreamID, and All returns them that way.
func NewSet(metas ...Meta) *Set {
	s := &Set{metas: make(map[StreamID]Meta, len(metas))}
	for _, m := range metas {
		if _, dup := s.metas[m.ID]; dup {
			panic("sample: duplicate stream id in set")
		}
		s.metas[m.ID] = m
		s.order = append(s.order, m.ID)
	}
	sortIDs(s.order)
	return s
}

// sortIDs orders stream IDs ascending. Insertion sort: a Set holds a handful of
// streams, and this keeps package sample free of imports as documented above.
func sortIDs(ids []StreamID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

// Has reports whether the device exposes the stream.
func (s *Set) Has(id StreamID) bool {
	_, ok := s.metas[id]
	return ok
}

// Meta returns the stream's metadata and whether the device exposes it.
func (s *Set) Meta(id StreamID) (Meta, bool) {
	m, ok := s.metas[id]
	return m, ok
}

// MetaFor returns the stream's metadata, panicking if the device does not
// expose it. Use it where the stream is known to be present — a chunk that came
// from this device, say — and Meta elsewhere.
func (s *Set) MetaFor(id StreamID) Meta {
	m, ok := s.metas[id]
	if !ok {
		panic("sample: stream not in set")
	}
	return m
}

// All returns the device's stream IDs in wire order.
func (s *Set) All() []StreamID {
	out := make([]StreamID, len(s.order))
	copy(out, s.order)
	return out
}

// Classic returns the stream set of a Muse 2 / Muse S (pre-Athena): 5 EEG
// channels including Right AUX, 3 PPG channels, and the two IMU streams.
func Classic() *Set {
	return NewSet(metas[EEG], metas[PPG], metas[ACC], metas[Gyro])
}

// Athena returns the stream set of a Muse S Athena (Gen 3).
//
// Its EEG is 4 channels, not 5: the headband reports 8 channels on tag 0x12 but
// only TP9/AF7/AF8/TP10 are electrode signals, matching what muse-lsl's
// athena.py forwards. It has no PPG characteristic — its pulse sensor is the
// 16-channel Optics stream instead.
//
// SamplesPerPacket is the largest a packet carries (tag 0x11 packs 4 EEG
// samples, 0x12 packs 2; optics tag 0x34 packs 3), since it is a sizing hint
// rather than a guarantee.
func Athena() *Set {
	return NewSet(
		Meta{
			ID:               EEG,
			Name:             "eeg",
			Channels:         []string{"TP9", "AF7", "AF8", "TP10"},
			Unit:             "uV",
			Rate:             256,
			SamplesPerPacket: 4,
		},
		Meta{
			ID:               ACC,
			Name:             "acc",
			Channels:         []string{"X", "Y", "Z"},
			Unit:             "g",
			Rate:             52,
			SamplesPerPacket: 3,
		},
		Meta{
			ID:               Gyro,
			Name:             "gyro",
			Channels:         []string{"X", "Y", "Z"},
			Unit:             "dps",
			Rate:             52,
			SamplesPerPacket: 3,
		},
		Meta{
			ID:   Optics,
			Name: "optics",
			// OPT0..OPT15 spelled out rather than generated, to keep this
			// package import-free as documented above. Three tags feed this
			// one canonical space (0x34 fills 4-7, 0x35 fills 0-7, 0x36 all
			// 16), so which of these carry signal depends on the preset; the
			// rest stay at zero. See muse.athenaOpticsIndex.
			Channels: []string{
				"OPT0", "OPT1", "OPT2", "OPT3",
				"OPT4", "OPT5", "OPT6", "OPT7",
				"OPT8", "OPT9", "OPT10", "OPT11",
				"OPT12", "OPT13", "OPT14", "OPT15",
			},
			// Raw counts, as with classic PPG: muse-lsl labels the unit
			// "a.u." and applies a scale of 1.0.
			Unit:             "",
			Rate:             64,
			SamplesPerPacket: 3,
		},
	)
}

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

// MetaFor returns the classic-headband metadata for a stream. It panics on an
// unknown ID, which can only happen if a caller invents one.
//
// Prefer Set.MetaFor where a device is in play: this describes a classic
// Muse 2 / Muse S and is wrong for an Athena, whose EEG is 4 channels wide.
func MetaFor(id StreamID) Meta {
	m, ok := metas[id]
	if !ok {
		panic("sample: unknown stream id")
	}
	return m
}

// All returns every stream ID in wire order. Like MetaFor, this is the classic
// layout; use Set.All for the streams a particular device actually has.
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
