package muse

// Muse S Athena (Gen 3) streaming.
//
// The Athena replaces the classic one-characteristic-per-channel layout with
// two characteristics (273e0013, 273e0014) carrying every sensor multiplexed
// together, tagged per block. Nothing about the classic path applies: the
// framing, the bit order, the scales and the start command all differ.
//
// Ported from muse-lsl's athena.py, which is itself a port of BrainFlow
// PR #779 (commit 7b2e41d3, MIT). muse-lsl remains gomuse's reference, as it is
// for the classic path — see the package comment in constants.go.
//
// Two differences from the classic protocol are worth stating plainly, because
// both make a classic-protocol probe look like a dead headband:
//
//   - Streaming starts with "dc001", not "d". Sending "d" answers {"rc":255}.
//   - Presets are ordinary length-prefixed strings ("p1041"), not the classic
//     fixed frame 0x04 'p' digits 0x0a, which answers {"rc":69}.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"

	"gomuse/internal/sample"
)

// Athena framing. A single BLE notification holds one or more packets laid end
// to end, each introduced by its own length byte:
//
//	[len][idx u16][6 bytes unused][tag][blk][payload ...]   <- 14-byte header
//	  then zero or more subpackets: [tag][idx][3 unused][payload ...]
//
// Only the packet index (bytes 1-2), the primary tag (byte 9) and the block
// index (byte 10) are decoded; the rest of the header is padding we have no use
// for, since timestamps come from host arrival rather than a device clock.
const (
	athenaHeaderSize    = 14
	athenaSubHeaderSize = 5
)

// Athena scale factors.
const (
	// AthenaEEGScale converts a 14-bit count to microvolts over a 1450 µV
	// range. Samples are centred on AthenaEEGMidpoint.
	AthenaEEGScale    = 1450.0 / 16383.0
	AthenaEEGMidpoint = 1 << 13

	// The accelerometer scale matches the classic headband. The gyroscope
	// scale is negative, which is not a typo: the Athena's gyro axes are
	// inverted relative to the classic part, and muse-lsl carries the same
	// sign.
	AthenaAccScale  = 0.0000610352
	AthenaGyroScale = -0.0074768

	AthenaOpticsScale = 1.0

	// AthenaBatteryScale converts the raw uint16 to percent.
	AthenaBatteryScale = 1.0 / 512.0
)

// AthenaDefaultPreset streams EEG, IMU and optics together. Presets are
// hexadecimal despite looking decimal: p1041 reports back as "ps":4161.
const AthenaDefaultPreset = "p1041"

// athenaValidPresets are the presets muse-lsl accepts for this headband.
var athenaValidPresets = map[string]bool{
	"p20": true, "p21": true, "p50": true, "p51": true, "p60": true, "p61": true,
	"p1034": true, "p1035": true, "p1041": true, "p1042": true, "p1043": true,
	"p1044": true, "p1045": true, "p1046": true, "p4129": true,
}

// athenaSensor describes one sensor tag: what it carries and how much of it.
type athenaSensor struct {
	kind     string // eeg, acc_gyro, optics, battery, unknown
	channels int
	samples  int
	rate     float64
	dataLen  int
	variable bool // payload runs to the end of the packet
}

// athenaSensors maps a block tag to its layout. Tags absent from this table end
// block iteration, since without a length there is no way to find the next one.
var athenaSensors = map[byte]athenaSensor{
	0x11: {"eeg", 4, 4, 256, 28, false},
	0x12: {"eeg", 8, 2, 256, 28, false},
	0x34: {"optics", 4, 3, 64, 30, false},
	0x35: {"optics", 8, 2, 64, 40, false},
	// Measured on a MuseS-1FF3 at the p1041 default: optics arrives as 0x36
	// only, one sample per packet across all 16 channels, at ~64.5 Hz. The
	// narrower tags are kept because other presets are undocumented, not
	// because they have been seen.
	0x36: {"optics", 16, 1, 64, 40, false},
	0x47: {"acc_gyro", 6, 3, 52, 36, false},
	0x53: {"unknown", 2, 6, 32, 24, false},
	0x88: {"battery", 1, 1, 0.2, 0, true},
	0x98: {"battery", 1, 1, 1, 20, false},
}

// splitAthenaPackets cuts a notification into its length-prefixed packets.
//
// A truncated or nonsensical length ends the scan rather than failing the whole
// notification: the packets already recovered are good, and the alternative is
// discarding live samples over a malformed tail.
func splitAthenaPackets(data []byte) [][]byte {
	var out [][]byte
	for off := 0; off < len(data); {
		if len(data)-off < athenaHeaderSize {
			break
		}
		n := int(data[off])
		if n < athenaHeaderSize || off+n > len(data) {
			break
		}
		out = append(out, data[off:off+n])
		off += n
	}
	return out
}

// athenaBlock is one sensor's slice of a packet.
type athenaBlock struct {
	tag     byte
	payload []byte
}

// athenaBlocks splits a packet into the primary block plus any subpackets.
func athenaBlocks(packet []byte) []athenaBlock {
	if len(packet) < athenaHeaderSize {
		return nil
	}
	var out []athenaBlock

	payload := packet[athenaHeaderSize:]
	offset := len(payload)

	if s, ok := athenaSensors[packet[9]]; ok {
		n := s.dataLen
		if s.variable || n > len(payload) {
			n = len(payload)
		}
		out = append(out, athenaBlock{tag: packet[9], payload: payload[:n]})
		offset = n
	}

	for offset+athenaSubHeaderSize <= len(payload) {
		tag := payload[offset]
		s, ok := athenaSensors[tag]
		if !ok {
			// No length for this tag, so the rest of the packet cannot be
			// walked. Stop rather than guess.
			break
		}
		remaining := len(payload) - offset - athenaSubHeaderSize
		n := s.dataLen
		if s.variable {
			n = remaining
		}
		if n <= 0 || n > remaining {
			break
		}
		start := offset + athenaSubHeaderSize
		out = append(out, athenaBlock{tag: tag, payload: payload[start : start+n]})
		offset = start + n
	}
	return out
}

// The packet's uint16 counter at bytes 1-2 is deliberately not decoded.
// Timestamps come from host arrival (see Dejitter.NextArrival) because the
// counter is shared across multiplexed streams and so cannot be used to detect
// a gap in any one of them; muse-lsl's athena.py ignores it for the same
// reason.

// extractLSBBits reads width bits starting at absolute bit index start,
// LSB-first within each byte: bit i lives at byte i/8, position i%8 counting
// from the least significant end.
//
// This is the opposite order from the classic protocol, whose bitReader in
// decode.go is MSB-first. The two are not interchangeable — reusing either for
// the other stream produces plausible-looking noise rather than an error.
func extractLSBBits(data []byte, start, width int) uint32 {
	var v uint32
	for i := 0; i < width; i++ {
		bit := start + i
		if byteIdx := bit / 8; byteIdx < len(data) {
			if (data[byteIdx]>>(bit%8))&1 == 1 {
				v |= 1 << i
			}
		}
	}
	return v
}

// decodeAthenaEEG decodes packed 14-bit LSB-first samples into channel-major
// microvolts, zero-centred by subtracting the midpoint.
//
// nchan is the packet's channel count, not the number of channels we publish:
// tag 0x12 packs 8, of which only the first four are electrodes. All of them
// must be walked so that the bit offsets of later samples stay correct.
func decodeAthenaEEG(data []byte, nchan, nsamp int) [][]float64 {
	out := make([][]float64, nchan)
	for ch := range out {
		out[ch] = make([]float64, nsamp)
	}
	for i := 0; i < nsamp; i++ {
		for ch := 0; ch < nchan; ch++ {
			raw := extractLSBBits(data, (i*nchan+ch)*14, 14)
			out[ch][i] = (float64(raw) - AthenaEEGMidpoint) * AthenaEEGScale
		}
	}
	return out
}

// decodeAthenaIMU decodes interleaved int16 little-endian samples into
// accelerometer and gyroscope triples: three acc axes then three gyro axes per
// sample.
func decodeAthenaIMU(data []byte, nsamp int) (acc, gyro [][]float64) {
	acc = make([][]float64, 3)
	gyro = make([][]float64, 3)
	for i := range acc {
		acc[i] = make([]float64, nsamp)
		gyro[i] = make([]float64, nsamp)
	}
	for i := 0; i < nsamp; i++ {
		for ch := 0; ch < 6; ch++ {
			off := (i*6 + ch) * 2
			if off+2 > len(data) {
				return acc, gyro
			}
			raw := int16(binary.LittleEndian.Uint16(data[off : off+2]))
			if ch < 3 {
				acc[ch][i] = float64(raw) * AthenaAccScale
			} else {
				gyro[ch-3][i] = float64(raw) * AthenaGyroScale
			}
		}
	}
	return acc, gyro
}

// AthenaOpticsChannels is the canonical optics channel count. Tags carry 4, 8
// or 16 channels into this shared space.
const AthenaOpticsChannels = 16

// athenaOpticsIndex maps a per-tag channel index onto a canonical optics
// channel, or -1 if the tag does not populate it.
func athenaOpticsIndex(tag byte, ch int) int {
	switch tag {
	case 0x34:
		if ch >= 0 && ch < 4 {
			return ch + 4
		}
	case 0x35:
		if ch >= 0 && ch < 8 {
			return ch
		}
	case 0x36:
		if ch >= 0 && ch < 16 {
			return ch
		}
	}
	return -1
}

// decodeAthenaOptics decodes packed 20-bit LSB-first optics samples into the
// canonical 16-channel space. Channels the tag does not carry stay zero.
//
// The output is always 16 channels wide regardless of how many the tag packs,
// so that sample.Optics has one fixed shape and a preset change cannot alter
// the channel count a viewer was already told about in its hello message.
func decodeAthenaOptics(data []byte, tag byte, nchan, nsamp int) [][]float64 {
	out := make([][]float64, AthenaOpticsChannels)
	for ch := range out {
		out[ch] = make([]float64, nsamp)
	}
	for i := 0; i < nsamp; i++ {
		for ch := 0; ch < nchan; ch++ {
			idx := athenaOpticsIndex(tag, ch)
			if idx < 0 {
				continue
			}
			raw := extractLSBBits(data, (i*nchan+ch)*20, 20)
			out[idx][i] = float64(raw) * AthenaOpticsScale
		}
	}
	return out
}

// decodeAthenaBattery reads the battery percentage from a battery block.
func decodeAthenaBattery(data []byte) (float64, bool) {
	if len(data) < 2 {
		return 0, false
	}
	return float64(binary.LittleEndian.Uint16(data[0:2])) * AthenaBatteryScale, true
}

// athenaStreamer turns Athena notifications into chunks. It owns one Dejitter
// per stream, because the streams are multiplexed together at different rates
// and each needs its own fit.
type athenaStreamer struct {
	publish func(sample.Chunk)
	touch   func()
	now     func() float64
	streams map[sample.StreamID]bool

	mu        sync.Mutex
	dejitters map[sample.StreamID]*Dejitter
	// batteryPct is only meaningful once batteryOK is set: a headband that has
	// not yet sent a battery block and one reporting a flat cell must not read
	// the same, and tag 0x88 arrives only every five seconds.
	batteryPct float64
	batteryOK  bool
}

func newAthenaStreamer(cfg Config, publish func(sample.Chunk), touch func(), now func() float64) *athenaStreamer {
	enabled := map[sample.StreamID]bool{
		sample.EEG:    cfg.wants(sample.EEG),
		sample.ACC:    cfg.wants(sample.ACC),
		sample.Gyro:   cfg.wants(sample.Gyro),
		sample.Optics: cfg.wants(sample.Optics),
	}
	return &athenaStreamer{
		publish:   publish,
		touch:     touch,
		now:       now,
		streams:   enabled,
		dejitters: map[sample.StreamID]*Dejitter{},
	}
}

// timestamps returns dejittered timestamps for n samples of a stream, creating
// that stream's fit on first use.
func (a *athenaStreamer) timestamps(id sample.StreamID, rate float64, n int, arrival float64) []float64 {
	d, ok := a.dejitters[id]
	if !ok {
		d = NewDejitter(rate, n, arrival)
		a.dejitters[id] = d
	}
	return d.NextArrival(n, arrival)
}

// battery returns the last reported battery percentage, and whether one has
// been reported at all.
func (a *athenaStreamer) battery() (float64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.batteryPct, a.batteryOK
}

// handleControl receives the headband's replies to control commands. They are
// ASCII fragments of a JSON document, length-prefixed and split across several
// notifications; gomuse does not reassemble them, since nothing it needs is in
// there. The subscription itself is what matters — see setupAthena.
func (a *athenaStreamer) handleControl([]byte) {}

// handle processes one BLE notification.
func (a *athenaStreamer) handle(data []byte) {
	packets := splitAthenaPackets(data)
	if len(packets) == 0 {
		return
	}
	a.touch()

	for _, p := range packets {
		arrival := a.now()
		for _, b := range athenaBlocks(p) {
			a.handleBlock(b, arrival)
		}
	}
}

func (a *athenaStreamer) handleBlock(b athenaBlock, arrival float64) {
	s, ok := athenaSensors[b.tag]
	if !ok {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	switch s.kind {
	case "battery":
		if pct, ok := decodeAthenaBattery(b.payload); ok {
			a.batteryPct, a.batteryOK = pct, true
		}

	case "eeg":
		if !a.streams[sample.EEG] {
			return
		}
		data := decodeAthenaEEG(b.payload, s.channels, s.samples)
		// Only the first four channels are electrodes; the rest of an 8-channel
		// packet is aux the outlet does not carry. See sample.Athena.
		nch := len(sample.Athena().MetaFor(sample.EEG).Channels)
		if len(data) > nch {
			data = data[:nch]
		}
		ts := a.timestamps(sample.EEG, s.rate, s.samples, arrival)
		a.publish(sample.Chunk{Stream: sample.EEG, Timestamps: ts, Data: data})

	case "acc_gyro":
		acc, gyro := decodeAthenaIMU(b.payload, s.samples)
		// Both streams share the packet, so they share its timestamps too;
		// each still keeps its own fit, since a viewer may take one and not
		// the other.
		if a.streams[sample.ACC] {
			ts := a.timestamps(sample.ACC, s.rate, s.samples, arrival)
			a.publish(sample.Chunk{Stream: sample.ACC, Timestamps: ts, Data: acc})
		}
		if a.streams[sample.Gyro] {
			ts := a.timestamps(sample.Gyro, s.rate, s.samples, arrival)
			a.publish(sample.Chunk{Stream: sample.Gyro, Timestamps: ts, Data: gyro})
		}

	case "optics":
		if !a.streams[sample.Optics] {
			return
		}
		// All three optics tags decode into the same canonical 16-channel
		// space, so the published chunk is the same width whichever tag
		// arrived; channels this tag does not carry stay zero.
		data := decodeAthenaOptics(b.payload, b.tag, s.channels, s.samples)
		ts := a.timestamps(sample.Optics, s.rate, s.samples, arrival)
		a.publish(sample.Chunk{Stream: sample.Optics, Timestamps: ts, Data: data})
	}
}

// setupAthena wires up an Athena headband: subscribe to both data
// characteristics, run the init sequence, then start streaming.
func (c *Client) setupAthena(chars map[string]bluetooth.DeviceCharacteristic, cfg Config) error {
	if !wantsAny(cfg, sample.EEG, sample.ACC, sample.Gyro, sample.Optics) {
		return errors.New("muse: none of the enabled streams are supported by this Athena headband (enable at least one of: eeg, acc, gyro, optics)")
	}

	ctrl, ok := chars[CharControl]
	if !ok {
		return errNoControl
	}
	c.control = ctrl
	c.streams = sample.Athena()

	// This headband has no PPG characteristic; optics is its pulse sensor. Say
	// so rather than silently dropping a stream the user asked for by name —
	// but only when they asked explicitly, since PPG is on by default and
	// nagging every Athena run would be noise.
	if cfg.wants(sample.PPG) && cfg.Streams != nil {
		if cfg.wants(sample.Optics) {
			c.warn("this headband has no PPG; streaming its 16-channel optics instead")
		} else {
			c.warn("this headband has no PPG; its pulse sensor is optics, which -optics=false disabled")
		}
	}

	streamer := newAthenaStreamer(cfg, c.publish, c.touch, nowSeconds)
	c.athena = streamer

	// Subscribe to control before the data characteristics, as athena.py does.
	// The headband will not start streaming for a client that is not listening
	// for its replies: without this the init sequence is accepted and "dc001"
	// answers {"rc":0}, but no notification ever arrives.
	if err := ctrl.EnableNotifications(streamer.handleControl); err != nil {
		return fmt.Errorf("subscribe to control: %w", err)
	}

	for _, uuid := range []string{CharAthenaData1, CharAthenaData2} {
		ch, ok := chars[uuid]
		if !ok {
			return fmt.Errorf("muse: Athena data characteristic %s not found", uuid)
		}
		if err := ch.EnableNotifications(streamer.handle); err != nil {
			return fmt.Errorf("subscribe to %s: %w", uuid, err)
		}
	}

	preset := cfg.Preset
	if preset == "" {
		preset = AthenaDefaultPreset
	}
	if preset[0] != 'p' {
		preset = "p" + preset
	}
	if !athenaValidPresets[preset] {
		c.warn(fmt.Sprintf("unknown Athena preset %s, using %s", preset, AthenaDefaultPreset))
		preset = AthenaDefaultPreset
	}

	// The init sequence, verbatim from muse-lsl's athena.py: version, status,
	// halt, preset, status. The pauses matter — the headband answers each
	// command and drops ones that arrive while it is still replying.
	for _, cmd := range []string{"v6", "s", "h", preset, "s"} {
		if err := c.writeString(cmd); err != nil {
			return fmt.Errorf("athena init %q: %w", cmd, err)
		}
		time.Sleep(athenaCommandGap)
	}

	if cfg.DisableLight {
		if err := c.writeString("L0"); err != nil {
			return fmt.Errorf("disable light: %w", err)
		}
		time.Sleep(athenaCommandGap)
	}

	// "dc001" starts streaming, and is sent twice: the first is unreliable
	// immediately after the preset change, exactly as in athena.py.
	for i := 0; i < 2; i++ {
		if err := c.writeString("dc001"); err != nil {
			return fmt.Errorf("start streaming: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := c.writeString("L1"); err != nil {
		return fmt.Errorf("enable low latency: %w", err)
	}
	time.Sleep(athenaCommandGap)
	return c.writeString("s")
}

// athenaCommandGap is the pause muse-lsl leaves between control commands.
const athenaCommandGap = 200 * time.Millisecond

// timing reports the fit of every stream that has produced a packet.
func (a *athenaStreamer) timing() []StreamTiming {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []StreamTiming
	for _, id := range sample.Athena().All() {
		if d, ok := a.dejitters[id]; ok {
			m := sample.MetaFor(id)
			out = append(out, d.timing(m.Name, m.Rate))
		}
	}
	return out
}
