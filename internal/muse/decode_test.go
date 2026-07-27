package muse

import (
	"math"
	"testing"
)

// eegPacket builds a 20-byte EEG notification from a packet index and 12
// 12-bit samples, packing them MSB-first exactly as the headband does.
// It is written independently of the production bit reader so that the two
// implementations have to agree.
func eegPacket(t *testing.T, index uint16, raw [12]uint16) []byte {
	t.Helper()
	p := make([]byte, PacketSize)
	p[0] = byte(index >> 8)
	p[1] = byte(index)
	// Two 12-bit samples fit exactly in three bytes.
	for i := 0; i < 6; i++ {
		a, b := raw[2*i]&0xFFF, raw[2*i+1]&0xFFF
		off := 2 + 3*i
		p[off] = byte(a >> 4)
		p[off+1] = byte(a&0x0F)<<4 | byte(b>>8)
		p[off+2] = byte(b)
	}
	return p
}

func expectEEG(t *testing.T, got []float64, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDecodeEEGAllZeros(t *testing.T) {
	p := make([]byte, PacketSize)
	p[0], p[1] = 0x01, 0x02

	idx, samples, err := DecodeEEG(p)
	if err != nil {
		t.Fatalf("DecodeEEG: %v", err)
	}
	if idx != 0x0102 {
		t.Errorf("index = %#x, want 0x0102", idx)
	}
	want := make([]float64, 12)
	for i := range want {
		want[i] = EEGScaleFactor * -EEGZeroOffset
	}
	expectEEG(t, samples, want)
}

func TestDecodeEEGAllOnes(t *testing.T) {
	p := make([]byte, PacketSize)
	for i := 2; i < PacketSize; i++ {
		p[i] = 0xFF
	}
	_, samples, err := DecodeEEG(p)
	if err != nil {
		t.Fatalf("DecodeEEG: %v", err)
	}
	want := make([]float64, 12)
	for i := range want {
		want[i] = EEGScaleFactor * (4095 - EEGZeroOffset)
	}
	expectEEG(t, samples, want)
}

// TestDecodeEEGStraddling pins sample ordering across the byte boundaries that
// 12-bit packing creates: only even-numbered samples start on a byte edge.
func TestDecodeEEGStraddling(t *testing.T) {
	raw := [12]uint16{0x001, 0x800, 0xFFF, 0x123, 0x456, 0x789, 0xABC, 0xDEF, 0x000, 0xFFF, 0x555, 0xAAA}
	p := eegPacket(t, 0xBEEF, raw)

	idx, samples, err := DecodeEEG(p)
	if err != nil {
		t.Fatalf("DecodeEEG: %v", err)
	}
	if idx != 0xBEEF {
		t.Errorf("index = %#x, want 0xBEEF", idx)
	}
	want := make([]float64, 12)
	for i, r := range raw {
		want[i] = EEGScaleFactor * (float64(r) - EEGZeroOffset)
	}
	expectEEG(t, samples, want)
}

// TestDecodeEEGKnownBytes checks the decoder against literal wire bytes, so a
// bug in the test's own packer cannot mask a bug in the decoder.
func TestDecodeEEGKnownBytes(t *testing.T) {
	p := []byte{
		0x00, 0x07,
		0x00, 0x18, 0x00, // 0x001, 0x800
		0xFF, 0xF1, 0x23, // 0xFFF, 0x123
		0x00, 0x00, 0x00,
		0x00, 0x00, 0x00,
		0x00, 0x00, 0x00,
		0x00, 0x00, 0x00,
	}
	idx, samples, err := DecodeEEG(p)
	if err != nil {
		t.Fatalf("DecodeEEG: %v", err)
	}
	if idx != 7 {
		t.Errorf("index = %d, want 7", idx)
	}
	raw := []uint16{0x001, 0x800, 0xFFF, 0x123, 0, 0, 0, 0, 0, 0, 0, 0}
	want := make([]float64, 12)
	for i, r := range raw {
		want[i] = EEGScaleFactor * (float64(r) - EEGZeroOffset)
	}
	expectEEG(t, samples, want)
}

func TestDecodeEEGWrongLength(t *testing.T) {
	for _, n := range []int{0, 19, 21} {
		if _, _, err := DecodeEEG(make([]byte, n)); err == nil {
			t.Errorf("len %d: expected error, got nil", n)
		}
	}
}

func TestDecodePPG(t *testing.T) {
	p := []byte{
		0x12, 0x34,
		0x12, 0x34, 0x56, // spans three bytes
		0xFF, 0xFF, 0xFF, // max uint24
		0x00, 0x00, 0x01,
		0x80, 0x00, 0x00,
		0x00, 0xFF, 0x00,
		0x00, 0x00, 0x00,
	}
	idx, samples, err := DecodePPG(p)
	if err != nil {
		t.Fatalf("DecodePPG: %v", err)
	}
	if idx != 0x1234 {
		t.Errorf("index = %#x, want 0x1234", idx)
	}
	want := []float64{0x123456, 0xFFFFFF, 1, 0x800000, 0xFF00, 0}
	if len(samples) != len(want) {
		t.Fatalf("got %d samples, want %d", len(samples), len(want))
	}
	for i := range want {
		if samples[i] != want[i] {
			t.Errorf("sample %d = %v, want %v", i, samples[i], want[i])
		}
	}
}

func TestDecodePPGWrongLength(t *testing.T) {
	if _, _, err := DecodePPG(make([]byte, 18)); err == nil {
		t.Error("expected error for short packet")
	}
}

// TestDecodeIMUSignExtension pins the classic bug: raw 0xFFFF is -1, not 65535.
func TestDecodeIMUSignExtension(t *testing.T) {
	raw := []uint16{
		0xFFFF, 0x0001, 0x8000, // sample 0
		0x7FFF, 0x0000, 0xFFFE, // sample 1
		0x0100, 0xFF00, 0x0002, // sample 2
	}
	p := make([]byte, PacketSize)
	p[0], p[1] = 0x00, 0x2A
	for i, v := range raw {
		p[2+2*i] = byte(v >> 8)
		p[3+2*i] = byte(v)
	}

	idx, samples, err := DecodeIMU(p, 1)
	if err != nil {
		t.Fatalf("DecodeIMU: %v", err)
	}
	if idx != 42 {
		t.Errorf("index = %d, want 42", idx)
	}
	want := [][3]float64{
		{-1, 1, -32768},
		{32767, 0, -2},
		{256, -256, 2},
	}
	if len(samples) != 3 {
		t.Fatalf("got %d samples, want 3", len(samples))
	}
	for i := range want {
		for j := 0; j < 3; j++ {
			if samples[i][j] != want[i][j] {
				t.Errorf("sample %d axis %d = %v, want %v", i, j, samples[i][j], want[i][j])
			}
		}
	}
}

func TestDecodeIMUScaling(t *testing.T) {
	p := make([]byte, PacketSize)
	// Every axis of every sample = -1.
	for i := 2; i < PacketSize; i++ {
		p[i] = 0xFF
	}
	_, samples, err := DecodeIMU(p, AccelerometerScaleFactor)
	if err != nil {
		t.Fatalf("DecodeIMU: %v", err)
	}
	for i := range samples {
		for j := 0; j < 3; j++ {
			if math.Abs(samples[i][j]-(-AccelerometerScaleFactor)) > 1e-15 {
				t.Fatalf("sample %d axis %d = %v, want %v", i, j, samples[i][j], -AccelerometerScaleFactor)
			}
		}
	}

	_, gyro, err := DecodeIMU(p, GyroScaleFactor)
	if err != nil {
		t.Fatalf("DecodeIMU: %v", err)
	}
	if math.Abs(gyro[0][0]-(-GyroScaleFactor)) > 1e-15 {
		t.Errorf("gyro scale not applied: %v", gyro[0][0])
	}
}

func TestDecodeIMUWrongLength(t *testing.T) {
	if _, _, err := DecodeIMU(make([]byte, 19), 1); err == nil {
		t.Error("expected error for short packet")
	}
}

func TestBitReader(t *testing.T) {
	// 0xAB 0xCD 0xEF = 1010 1011 1100 1101 1110 1111
	r := bitReader{data: []byte{0xAB, 0xCD, 0xEF}}
	if got := r.read(4); got != 0xA {
		t.Errorf("read(4) = %#x, want 0xa", got)
	}
	if got := r.read(12); got != 0xBCD {
		t.Errorf("read(12) = %#x, want 0xbcd", got)
	}
	if got := r.read(8); got != 0xEF {
		t.Errorf("read(8) = %#x, want 0xef", got)
	}

	r2 := bitReader{data: []byte{0xFF, 0xFF, 0xFF}}
	if got := r2.read(24); got != 0xFFFFFF {
		t.Errorf("read(24) = %#x, want 0xffffff", got)
	}
}
