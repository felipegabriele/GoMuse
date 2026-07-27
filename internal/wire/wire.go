// Package wire defines the binary frame format used to push sample chunks to
// the browser over the WebSocket, and the JSON control message the browser
// sends back.
//
// A frame is little-endian: a fixed 16-byte header followed by the samples.
//
//	offset size  field
//	0      u8    stream id (sample.StreamID: 0=EEG, 1=PPG, 2=ACC, 3=GYRO)
//	1      u8    nchan
//	2      u16   nsamples
//	4      f64   t0    Unix seconds of the first sample
//	12     f32   dt    sample period in seconds
//	16     f32[nchan*nsamples]  channel-major: all of ch0, then all of ch1, ...
//
// The mix of float widths is deliberate, not an oversight: t0 is f64 because a
// Unix epoch timestamp is ~1.7e9 and an f32 there has a resolution of about
// 128 ms, which would destroy sample timing. The samples themselves are f32
// because EEG microvolts carry ~7 significant digits of meaning at most, and
// halving the payload matters when frames arrive 21 times a second per stream.
// Do not "fix" this by making both the same width.
package wire

import (
	"encoding/binary"
	"fmt"
	"math"

	"gomuse/internal/sample"
)

// HeaderSize is the size in bytes of the fixed frame header.
const HeaderSize = 16

// Control is the JSON message the browser sends back to the server. Fields are
// pointers so that an absent field is distinguishable from an explicit false
// or 0: a client can change the window without touching the filter setting.
type Control struct {
	Filter *bool    `json:"filter,omitempty"`
	Window *float64 `json:"window,omitempty"`
}

// dtFor returns the sample period to advertise for a chunk. With more than one
// sample it is measured from the chunk's own timestamps, so the browser sees
// the dejittered real rate rather than the nominal one; with a single sample
// there is nothing to measure and we fall back to the nominal period.
func dtFor(c sample.Chunk) float64 {
	if n := c.NumSamples(); n > 1 {
		return (c.Timestamps[n-1] - c.Timestamps[0]) / float64(n-1)
	}
	return sample.MetaFor(c.Stream).Period()
}

// EncodeChunk serialises a chunk into a single frame.
//
// The chunk is assumed to be well formed: every channel slice as long as
// Timestamps, as produced by the BLE decoder.
func EncodeChunk(c sample.Chunk) []byte {
	nchan := c.NumChannels()
	nsamp := c.NumSamples()

	b := make([]byte, HeaderSize+4*nchan*nsamp)
	b[0] = byte(c.Stream)
	b[1] = byte(nchan)
	binary.LittleEndian.PutUint16(b[2:4], uint16(nsamp))

	var t0 float64
	if nsamp > 0 {
		t0 = c.Timestamps[0]
	}
	binary.LittleEndian.PutUint64(b[4:12], math.Float64bits(t0))
	binary.LittleEndian.PutUint32(b[12:16], math.Float32bits(float32(dtFor(c))))

	off := HeaderSize
	for ch := 0; ch < nchan; ch++ {
		for i := 0; i < nsamp; i++ {
			binary.LittleEndian.PutUint32(b[off:off+4], math.Float32bits(float32(c.Data[ch][i])))
			off += 4
		}
	}
	return b
}

// DecodeChunk parses a frame produced by EncodeChunk. It is used by the tests
// and by any Go client of the WebSocket.
//
// Timestamps are reconstructed as t0 + i*dt: the frame carries the period, not
// each individual instant, so a chunk that went in with jittered timestamps
// comes back evenly spaced.
func DecodeChunk(b []byte) (sample.Chunk, error) {
	var c sample.Chunk
	if len(b) < HeaderSize {
		return c, fmt.Errorf("wire: frame too short: %d bytes, need at least %d", len(b), HeaderSize)
	}

	id := sample.StreamID(b[0])
	var known bool
	for _, s := range sample.All() {
		if s == id {
			known = true
			break
		}
	}
	if !known {
		return c, fmt.Errorf("wire: unknown stream id %d", b[0])
	}

	nchan := int(b[1])
	if nchan == 0 {
		return c, fmt.Errorf("wire: zero channels")
	}
	if want := sample.MetaFor(id).NumChannels(); nchan != want {
		return c, fmt.Errorf("wire: stream %s has %d channels, frame declares %d",
			sample.MetaFor(id).Name, want, nchan)
	}
	nsamp := int(binary.LittleEndian.Uint16(b[2:4]))

	payload := b[HeaderSize:]
	if want := 4 * nchan * nsamp; len(payload) != want {
		return c, fmt.Errorf("wire: payload is %d bytes, header implies %d (%d channels x %d samples)",
			len(payload), want, nchan, nsamp)
	}

	t0 := math.Float64frombits(binary.LittleEndian.Uint64(b[4:12]))
	dt := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[12:16])))

	c.Stream = id
	c.Timestamps = make([]float64, nsamp)
	for i := range c.Timestamps {
		c.Timestamps[i] = t0 + float64(i)*dt
	}
	c.Data = make([][]float64, nchan)
	off := 0
	for ch := 0; ch < nchan; ch++ {
		c.Data[ch] = make([]float64, nsamp)
		for i := 0; i < nsamp; i++ {
			c.Data[ch][i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(payload[off : off+4])))
			off += 4
		}
	}
	return c, nil
}
