package wire

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"

	"gomuse/internal/sample"
)

// makeChunk builds a chunk for id at the nominal rate, with values that are
// distinct per (channel, sample) so a transposed encoding would be caught.
func makeChunk(id sample.StreamID, t0 float64) sample.Chunk {
	m := sample.MetaFor(id)
	n := m.SamplesPerPacket
	c := sample.Chunk{
		Stream:     id,
		Timestamps: make([]float64, n),
		Data:       make([][]float64, m.NumChannels()),
	}
	for i := range c.Timestamps {
		c.Timestamps[i] = t0 + float64(i)*m.Period()
	}
	for ch := range c.Data {
		c.Data[ch] = make([]float64, n)
		for i := 0; i < n; i++ {
			c.Data[ch][i] = float64(ch)*100 + float64(i) + 0.5
		}
	}
	return c
}

func TestRoundTripAllStreams(t *testing.T) {
	const t0 = 1753526400.123456 // needs f64: seconds since the epoch
	for _, id := range sample.All() {
		m := sample.MetaFor(id)
		in := makeChunk(id, t0)

		out, err := DecodeChunk(EncodeChunk(in))
		if err != nil {
			t.Fatalf("%s: DecodeChunk: %v", m.Name, err)
		}
		if out.Stream != id {
			t.Errorf("%s: stream = %d, want %d", m.Name, out.Stream, id)
		}
		if out.NumChannels() != m.NumChannels() {
			t.Fatalf("%s: channels = %d, want %d", m.Name, out.NumChannels(), m.NumChannels())
		}
		if out.NumSamples() != m.SamplesPerPacket {
			t.Fatalf("%s: samples = %d, want %d", m.Name, out.NumSamples(), m.SamplesPerPacket)
		}
		// t0 is f64 on the wire, so it must survive bit-exactly.
		if out.Timestamps[0] != in.Timestamps[0] {
			t.Errorf("%s: t0 = %.9f, want %.9f exactly", m.Name, out.Timestamps[0], in.Timestamps[0])
		}
		// Samples are f32 on the wire: compare with tolerance.
		for ch := range in.Data {
			for i := range in.Data[ch] {
				got, want := out.Data[ch][i], in.Data[ch][i]
				if math.Abs(got-want) > 1e-3 {
					t.Errorf("%s: Data[%d][%d] = %v, want %v", m.Name, ch, i, got, want)
				}
			}
		}
		// Reconstructed timestamps follow t0 + i*dt.
		for i := range out.Timestamps {
			want := in.Timestamps[0] + float64(i)*m.Period()
			if math.Abs(out.Timestamps[i]-want) > 1e-6 {
				t.Errorf("%s: Timestamps[%d] = %.9f, want %.9f", m.Name, i, out.Timestamps[i], want)
			}
		}
	}
}

func TestHeaderByteLayout(t *testing.T) {
	c := sample.Chunk{
		Stream:     sample.ACC,
		Timestamps: []float64{1000.5, 1001.5, 1002.5},
		Data: [][]float64{
			{1, 2, 3},
			{4, 5, 6},
			{7, 8, 9},
		},
	}
	b := EncodeChunk(c)

	if got, want := len(b), HeaderSize+4*3*3; got != want {
		t.Fatalf("len = %d, want %d", got, want)
	}
	if b[0] != byte(sample.ACC) {
		t.Errorf("byte 0 (stream) = %d, want %d", b[0], sample.ACC)
	}
	if b[1] != 3 {
		t.Errorf("byte 1 (nchan) = %d, want 3", b[1])
	}
	if n := binary.LittleEndian.Uint16(b[2:4]); n != 3 {
		t.Errorf("bytes 2:4 (nsamples) = %d, want 3", n)
	}
	if v := math.Float64frombits(binary.LittleEndian.Uint64(b[4:12])); v != 1000.5 {
		t.Errorf("bytes 4:12 (t0) = %v, want 1000.5", v)
	}
	if v := math.Float32frombits(binary.LittleEndian.Uint32(b[12:16])); v != 1 {
		t.Errorf("bytes 12:16 (dt) = %v, want 1", v)
	}
	// Channel-major payload: all of ch0, then ch1, then ch2.
	want := []float32{1, 2, 3, 4, 5, 6, 7, 8, 9}
	for i, w := range want {
		off := HeaderSize + 4*i
		if v := math.Float32frombits(binary.LittleEndian.Uint32(b[off : off+4])); v != w {
			t.Errorf("payload[%d] = %v, want %v", i, v, w)
		}
	}
}

func TestDtFromTimestampsNotNominalRate(t *testing.T) {
	// EEG's nominal period is 1/256 = 0.00390625. Feed a slightly slower
	// actual rate and require the encoder to report the measured one.
	const actual = 0.004
	c := makeChunk(sample.EEG, 500)
	for i := range c.Timestamps {
		c.Timestamps[i] = 500 + float64(i)*actual
	}
	b := EncodeChunk(c)
	dt := math.Float32frombits(binary.LittleEndian.Uint32(b[12:16]))
	if math.Abs(float64(dt)-actual) > 1e-9 {
		t.Fatalf("dt = %v, want %v (measured, not nominal)", dt, actual)
	}
	if math.Abs(float64(dt)-sample.MetaFor(sample.EEG).Period()) < 1e-9 {
		t.Fatalf("dt fell back to the nominal period")
	}
}

func TestDtFallsBackToNominalForSingleSample(t *testing.T) {
	c := sample.Chunk{
		Stream:     sample.PPG,
		Timestamps: []float64{7},
		Data:       [][]float64{{1}, {2}, {3}},
	}
	b := EncodeChunk(c)
	dt := math.Float32frombits(binary.LittleEndian.Uint32(b[12:16]))
	want := float32(sample.MetaFor(sample.PPG).Period())
	if dt != want {
		t.Fatalf("dt = %v, want nominal %v", dt, want)
	}
}

func TestDecodeErrors(t *testing.T) {
	good := EncodeChunk(makeChunk(sample.EEG, 100))

	t.Run("empty", func(t *testing.T) {
		if _, err := DecodeChunk(nil); err == nil {
			t.Fatal("want error for empty buffer")
		}
	})
	t.Run("truncated header", func(t *testing.T) {
		if _, err := DecodeChunk(good[:HeaderSize-1]); err == nil {
			t.Fatal("want error for truncated header")
		}
	})
	t.Run("truncated payload", func(t *testing.T) {
		if _, err := DecodeChunk(good[:len(good)-1]); err == nil {
			t.Fatal("want error for truncated payload")
		}
	})
	t.Run("payload too long", func(t *testing.T) {
		if _, err := DecodeChunk(append(append([]byte{}, good...), 0, 0, 0, 0)); err == nil {
			t.Fatal("want error for over-long payload")
		}
	})
	t.Run("bad stream id", func(t *testing.T) {
		b := append([]byte{}, good...)
		b[0] = 9
		if _, err := DecodeChunk(b); err == nil {
			t.Fatal("want error for unknown stream id")
		}
	})
	t.Run("zero channels", func(t *testing.T) {
		b := append([]byte{}, good...)
		b[1] = 0
		if _, err := DecodeChunk(b); err == nil {
			t.Fatal("want error for zero channels")
		}
	})
}

func TestControlJSON(t *testing.T) {
	var c Control
	if err := json.Unmarshal([]byte(`{"filter":false}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Filter == nil || *c.Filter != false {
		t.Fatalf("Filter = %v, want pointer to false", c.Filter)
	}
	if c.Window != nil {
		t.Fatalf("Window = %v, want nil (absent)", c.Window)
	}

	b, err := json.Marshal(Control{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Fatalf("empty Control marshals to %s, want {}", b)
	}
}
