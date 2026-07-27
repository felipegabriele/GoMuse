package muse

import (
	"encoding/hex"
	"math"
	"testing"

	"gomuse/internal/sample"
)

// Packets captured from a real Muse S Athena (MuseS-1FF3, firmware 3.1.23) on
// preset p1041. The expected values below were produced by running muse-lsl's
// athena.py over these exact bytes, so the tests pin gomuse against the
// reference implementation rather than against itself.
const (
	// A notification whose primary block is EEG tag 0x12, followed by two more
	// 0x12 subpackets.
	athenaEEGPacketHex = "6c0000a0fb608c0100120060bd28ffdfff07000280ffdffff7ff01801ea00d38" +
		"00f67e3fa00f6804927f12018dee29ef9fda47f3598026dfcac7f0817df1a075f837da88e0217438204" +
		"28a120227ef2b7e208397383d4d53dc1bf7c1014ab8afc6f8bcf19a0f16914540cd46"

	// A notification whose primary block is IMU tag 0x47, followed by an EEG
	// subpacket and a 0x53 block.
	athenaIMUPacketHex = "701c00a2fb608c01004706c40110e4070103dbbf3d003b00b8ffef072c039fbf0e00" +
		"0f00d7ff8907e70291bffdffdbffd6ff1238bd8d19f6ebad2981458dfa1dca97db49b9c9a5e5870effff" +
		"6f5bd7d68c85c55301bd8d190b20ef1f0d20cb1f1b20bb220c20ca20122082210d20e520"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	return b
}

// closeEnough compares against the Python reference, which computes in float32
// and prints ten places. A tolerance well below the LSB of a 14-bit sample
// (0.0885 µV) is tight enough to catch any real decode error.
func closeEnough(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

func TestSplitAthenaPacketsFramesRealNotification(t *testing.T) {
	data := mustHex(t, athenaEEGPacketHex)
	packets := splitAthenaPackets(data)
	if len(packets) != 1 {
		t.Fatalf("got %d packets, want 1", len(packets))
	}
	if got, want := len(packets[0]), int(data[0]); got != want {
		t.Errorf("packet length = %d, want the framed %d", got, want)
	}
}

func TestSplitAthenaPacketsStopsOnGarbage(t *testing.T) {
	// A length byte below the header size cannot be walked; the scan must stop
	// rather than loop or panic.
	if got := splitAthenaPackets([]byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); len(got) != 0 {
		t.Errorf("got %d packets from an undersized length byte, want 0", len(got))
	}
	// A length running past the buffer is a truncated notification.
	trunc := make([]byte, 20)
	trunc[0] = 200
	if got := splitAthenaPackets(trunc); len(got) != 0 {
		t.Errorf("got %d packets from a truncated notification, want 0", len(got))
	}
	if got := splitAthenaPackets(nil); len(got) != 0 {
		t.Errorf("got %d packets from nil, want 0", len(got))
	}
}

func TestAthenaBlocksWalksSubpackets(t *testing.T) {
	// muse-lsl reports this notification as three 0x12 blocks of 28 bytes: the
	// primary plus two subpackets.
	blocks := athenaBlocks(mustHex(t, athenaEEGPacketHex))
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3", len(blocks))
	}
	for i, b := range blocks {
		if b.tag != 0x12 {
			t.Errorf("block %d tag = 0x%02x, want 0x12", i, b.tag)
		}
		if len(b.payload) != 28 {
			t.Errorf("block %d payload = %d bytes, want 28", i, len(b.payload))
		}
	}
}

func TestAthenaBlocksMixedTags(t *testing.T) {
	// The IMU notification carries three different sensors in one packet,
	// which is the whole point of the multiplexed protocol.
	blocks := athenaBlocks(mustHex(t, athenaIMUPacketHex))
	want := []struct {
		tag byte
		n   int
	}{{0x47, 36}, {0x12, 28}, {0x53, 24}}

	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d", len(blocks), len(want))
	}
	for i, w := range want {
		if blocks[i].tag != w.tag {
			t.Errorf("block %d tag = 0x%02x, want 0x%02x", i, blocks[i].tag, w.tag)
		}
		if len(blocks[i].payload) != w.n {
			t.Errorf("block %d payload = %d bytes, want %d", i, len(blocks[i].payload), w.n)
		}
	}
}

func TestDecodeAthenaEEGMatchesMuseLSL(t *testing.T) {
	packet := mustHex(t, athenaEEGPacketHex)
	payload := athenaBlocks(packet)[0].payload

	got := decodeAthenaEEG(payload, 8, 2)
	if len(got) != 8 {
		t.Fatalf("got %d channels, want 8", len(got))
	}

	// Electrode channels only; muse-lsl forwards the same four.
	want := [][]float64{
		{-0.0885063782, 2.6551914215},
		{-0.0885063782, 4.7793445587},
		{0.0000000000, 0.2655191422},
		{0.0000000000, -5.9299273491},
	}
	for ch, w := range want {
		for i, v := range w {
			if !closeEnough(got[ch][i], v, 1e-6) {
				t.Errorf("eeg[%d][%d] = %.10f, want %.10f", ch, i, got[ch][i], v)
			}
		}
	}
}

func TestDecodeAthenaIMUMatchesMuseLSL(t *testing.T) {
	packet := mustHex(t, athenaIMUPacketHex)
	payload := athenaBlocks(packet)[0].payload

	acc, gyro := decodeAthenaIMU(payload, 3)

	wantAcc := [][]float64{
		{0.1232911050, 0.1239624918, 0.1177368984},
		{0.0469360687, 0.0495605841, 0.0453491546},
		{-1.0022590160, -1.0059211254, -1.0067756176},
	}
	wantGyro := [][]float64{
		{-0.4560847878, -0.1046752036, 0.0224303994},
		{-0.4411312044, -0.1121520028, 0.2766416073},
		{0.5383296013, 0.3065488040, 0.3140256107},
	}
	for ax := range wantAcc {
		for i := range wantAcc[ax] {
			if !closeEnough(acc[ax][i], wantAcc[ax][i], 1e-6) {
				t.Errorf("acc[%d][%d] = %.10f, want %.10f", ax, i, acc[ax][i], wantAcc[ax][i])
			}
			if !closeEnough(gyro[ax][i], wantGyro[ax][i], 1e-6) {
				t.Errorf("gyro[%d][%d] = %.10f, want %.10f", ax, i, gyro[ax][i], wantGyro[ax][i])
			}
		}
	}

	// Z reads about -1 g: the headband was sitting still, so gravity is the
	// whole signal. A bit-order or scale error would not land here.
	if z := acc[2][0]; z > -0.9 || z < -1.1 {
		t.Errorf("acc Z = %.4f g, want roughly -1 (gravity)", z)
	}
}

func TestExtractLSBBitsIsLeastSignificantFirst(t *testing.T) {
	// Bit 0 is the low bit of byte 0, so 0x01 reads as 1 and 0x80 as 128.
	if got := extractLSBBits([]byte{0x01}, 0, 8); got != 1 {
		t.Errorf("extractLSBBits(0x01) = %d, want 1", got)
	}
	if got := extractLSBBits([]byte{0x80}, 0, 8); got != 128 {
		t.Errorf("extractLSBBits(0x80) = %d, want 128", got)
	}
	// A 14-bit field spanning a byte boundary: bits 0..13 of 0xFF 0x3F.
	if got := extractLSBBits([]byte{0xFF, 0x3F}, 0, 14); got != 0x3FFF {
		t.Errorf("extractLSBBits spanning bytes = %#x, want 0x3fff", got)
	}
	// Reading past the end yields zeroes rather than panicking, so a truncated
	// payload degrades instead of taking the stream down.
	if got := extractLSBBits([]byte{0xFF}, 8, 14); got != 0 {
		t.Errorf("extractLSBBits past end = %d, want 0", got)
	}
}

func TestAthenaOpticsIndexMapping(t *testing.T) {
	// Tag 0x34 carries the upper half of the canonical space.
	if got := athenaOpticsIndex(0x34, 0); got != 4 {
		t.Errorf("optics index for tag 0x34 ch0 = %d, want 4", got)
	}
	if got := athenaOpticsIndex(0x35, 0); got != 0 {
		t.Errorf("optics index for tag 0x35 ch0 = %d, want 0", got)
	}
	if got := athenaOpticsIndex(0x36, 15); got != 15 {
		t.Errorf("optics index for tag 0x36 ch15 = %d, want 15", got)
	}
	// Out-of-range channels and unknown tags map nowhere.
	if got := athenaOpticsIndex(0x34, 9); got != -1 {
		t.Errorf("optics index for out-of-range channel = %d, want -1", got)
	}
	if got := athenaOpticsIndex(0x11, 0); got != -1 {
		t.Errorf("optics index for a non-optics tag = %d, want -1", got)
	}
}

// fakeClock advances a fixed step per call, so streaming minutes of packets
// costs nothing and timestamps are deterministic.
func fakeClock(start, step float64) func() float64 {
	t := start - step
	return func() float64 {
		t += step
		return t
	}
}

func TestAthenaStreamerPublishesEEG(t *testing.T) {
	var got []sample.Chunk
	s := newAthenaStreamer(
		Config{Streams: map[sample.StreamID]bool{sample.EEG: true}},
		func(c sample.Chunk) { got = append(got, c) },
		func() {},
		fakeClock(1000, 0.0078125),
	)

	s.handle(mustHex(t, athenaEEGPacketHex))

	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3 (one per EEG block)", len(got))
	}
	for i, c := range got {
		if c.Stream != sample.EEG {
			t.Errorf("chunk %d stream = %d, want EEG", i, c.Stream)
		}
		// Four electrodes published, not the eight the packet carries.
		if c.NumChannels() != 4 {
			t.Errorf("chunk %d has %d channels, want 4", i, c.NumChannels())
		}
		if c.NumSamples() != 2 {
			t.Errorf("chunk %d has %d samples, want 2", i, c.NumSamples())
		}
		for ch, col := range c.Data {
			if len(col) != c.NumSamples() {
				t.Errorf("chunk %d channel %d has %d samples, want %d",
					i, ch, len(col), c.NumSamples())
			}
		}
	}

	// Timestamps must advance strictly, across chunk boundaries too: the
	// viewer and the recorder both assume a monotonic series.
	var last float64 = -1
	for i, c := range got {
		for j, ts := range c.Timestamps {
			if ts <= last {
				t.Fatalf("timestamp went backwards at chunk %d sample %d: %f <= %f", i, j, ts, last)
			}
			last = ts
		}
	}
}

func TestAthenaStreamerSplitsIMUIntoAccAndGyro(t *testing.T) {
	var got []sample.Chunk
	s := newAthenaStreamer(
		Config{Streams: map[sample.StreamID]bool{sample.ACC: true, sample.Gyro: true}},
		func(c sample.Chunk) { got = append(got, c) },
		func() {},
		fakeClock(1000, 0.019230769),
	)

	s.handle(mustHex(t, athenaIMUPacketHex))

	var acc, gyro int
	for _, c := range got {
		switch c.Stream {
		case sample.ACC:
			acc++
		case sample.Gyro:
			gyro++
		default:
			t.Errorf("unexpected stream %d published with EEG disabled", c.Stream)
		}
		if c.NumChannels() != 3 {
			t.Errorf("stream %d has %d channels, want 3", c.Stream, c.NumChannels())
		}
	}
	if acc != 1 || gyro != 1 {
		t.Errorf("got %d acc and %d gyro chunks, want 1 each", acc, gyro)
	}
}

func TestAthenaStreamerHonoursDisabledStreams(t *testing.T) {
	var got []sample.Chunk
	s := newAthenaStreamer(
		Config{Streams: map[sample.StreamID]bool{sample.ACC: true}},
		func(c sample.Chunk) { got = append(got, c) },
		func() {},
		fakeClock(1000, 0.02),
	)

	s.handle(mustHex(t, athenaIMUPacketHex))

	for _, c := range got {
		if c.Stream != sample.ACC {
			t.Errorf("published stream %d, but only ACC was enabled", c.Stream)
		}
	}
	if len(got) == 0 {
		t.Error("nothing published for the enabled ACC stream")
	}
}

func TestAthenaStreamerIgnoresGarbage(t *testing.T) {
	published := 0
	s := newAthenaStreamer(
		Config{},
		func(sample.Chunk) { published++ },
		func() {},
		fakeClock(1000, 0.01),
	)

	// A malformed notification must not publish or panic: BLE callbacks that
	// crash take the whole stream with them.
	s.handle(nil)
	s.handle([]byte{0x00})
	s.handle([]byte{0xFF, 0x01, 0x02})
	if published != 0 {
		t.Errorf("published %d chunks from garbage, want 0", published)
	}
}

func TestAthenaStreamerTracksBattery(t *testing.T) {
	s := newAthenaStreamer(Config{}, func(sample.Chunk) {}, func() {}, fakeClock(1000, 0.01))
	if got := s.battery(); got != 0 {
		t.Errorf("battery before any packet = %f, want 0", got)
	}

	// Tag 0x98 battery block: a uint16 scaled by 1/512.
	packet := make([]byte, athenaHeaderSize+20)
	packet[0] = byte(len(packet))
	packet[9] = 0x98
	packet[athenaHeaderSize] = 0x00
	packet[athenaHeaderSize+1] = 0x9C // 0x9C00 = 39936 -> 78%
	s.handle(packet)

	if got := s.battery(); !closeEnough(got, 78, 0.01) {
		t.Errorf("battery = %f, want about 78", got)
	}
}

func TestNextArrivalIsMonotonicAcrossVaryingPacketSizes(t *testing.T) {
	// Athena EEG packets carry 4 samples on tag 0x11 and 2 on 0x12, so the
	// dejitter must cope with the count changing between calls.
	d := NewDejitter(256, 4, 1000)

	var last float64 = -1
	arrival := 1000.0
	for i, n := range []int{4, 2, 2, 4, 2} {
		arrival += float64(n) / 256
		ts := d.NextArrival(n, arrival)
		if len(ts) != n {
			t.Fatalf("call %d returned %d timestamps, want %d", i, len(ts), n)
		}
		for _, v := range ts {
			if v <= last {
				t.Fatalf("call %d: timestamp %f did not advance past %f", i, v, last)
			}
			last = v
		}
	}

	if got := d.NextArrival(0, arrival); got != nil {
		t.Errorf("NextArrival(0) = %v, want nil", got)
	}
}
