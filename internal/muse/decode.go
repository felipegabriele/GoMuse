package muse

import "fmt"

// ErrPacketSize is returned when a notification is not exactly PacketSize
// bytes. The headband always sends fixed-length packets, so a short or long
// one means the buffer was mangled somewhere above us; decoding it anyway
// would silently produce garbage samples.
var ErrPacketSize = fmt.Errorf("muse: packet must be %d bytes", PacketSize)

// bitReader walks a byte slice MSB-first with no byte alignment between
// reads. The Muse packs samples continuously, so a 12-bit EEG sample can and
// does begin mid-byte; reading through a cursor keeps that concern in one
// place instead of spread across per-format shift expressions.
type bitReader struct {
	data []byte
	pos  int // next bit to read, counted from the MSB of data[0]
}

// read consumes the next n bits (n <= 32) and returns them right-aligned.
// Reading past the end yields zero bits; callers size their packets up front,
// so the decoders never rely on that.
func (r *bitReader) read(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		byteIdx := r.pos >> 3
		var bit uint32
		if byteIdx < len(r.data) {
			// Bit 0 of a byte on the wire is its most significant bit.
			shift := 7 - uint(r.pos&7)
			bit = uint32(r.data[byteIdx]>>shift) & 1
		}
		v = v<<1 | bit
		r.pos++
	}
	return v
}

// packetIndex reads the leading big-endian uint16 counter shared by every
// packet format and returns a reader positioned on the payload.
func packetIndex(packet []byte) (uint16, *bitReader) {
	index := uint16(packet[0])<<8 | uint16(packet[1])
	return index, &bitReader{data: packet, pos: 16}
}

// DecodeEEG returns the packet index and 12 samples in microvolts.
//
// The payload is 12 unsigned 12-bit samples, which is exactly the 18 bytes
// that follow the index.
func DecodeEEG(packet []byte) (uint16, []float64, error) {
	if len(packet) != PacketSize {
		return 0, nil, ErrPacketSize
	}
	index, r := packetIndex(packet)
	samples := make([]float64, 12)
	for i := range samples {
		samples[i] = EEGScaleFactor * (float64(r.read(12)) - EEGZeroOffset)
	}
	return index, samples, nil
}

// DecodePPG returns the packet index and 6 raw samples.
//
// PPG counts have no documented conversion to a physical unit, so they are
// returned as-is; downstream filtering works on the raw scale.
func DecodePPG(packet []byte) (uint16, []float64, error) {
	if len(packet) != PacketSize {
		return 0, nil, ErrPacketSize
	}
	index, r := packetIndex(packet)
	samples := make([]float64, 6)
	for i := range samples {
		samples[i] = float64(r.read(24))
	}
	return index, samples, nil
}

// DecodeIMU returns the packet index and 3 samples of (X, Y, Z), scaled by
// scale (AccelerometerScaleFactor or GyroScaleFactor).
//
// The payload is 9 signed 16-bit values laid out as three consecutive time
// samples of an XYZ triple. The values are signed, so the raw bits must be
// sign-extended before conversion: 0xFFFF is -1, not 65535.
func DecodeIMU(packet []byte, scale float64) (uint16, [][3]float64, error) {
	if len(packet) != PacketSize {
		return 0, nil, ErrPacketSize
	}
	index, r := packetIndex(packet)
	samples := make([][3]float64, 3)
	for i := range samples {
		for axis := 0; axis < 3; axis++ {
			samples[i][axis] = float64(int16(r.read(16))) * scale
		}
	}
	return index, samples, nil
}
