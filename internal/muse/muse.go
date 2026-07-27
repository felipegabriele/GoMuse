package muse

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"

	"gomuse/internal/sample"
)

// adapter is the process-wide BLE adapter. CoreBluetooth allows only one
// central manager per process, so this is enabled once and shared.
var (
	adapter     = bluetooth.DefaultAdapter
	adapterOnce sync.Once
	adapterErr  error
)

func enableAdapter() error {
	adapterOnce.Do(func() { adapterErr = adapter.Enable() })
	return adapterErr
}

// Found is a Muse discovered by a scan.
type Found struct {
	// Name is the advertised local name, e.g. "MuseS-1234".
	Name string
	// Address on macOS is a CoreBluetooth peripheral UUID, not a MAC address.
	// It is assigned per-host, so it is stable on this Mac but meaningless
	// on another one.
	Address string
	RSSI    int16
}

// Scan discovers Muse headbands until the context is cancelled or timeout
// elapses, returning them sorted by descending signal strength.
//
// Devices are matched on an advertised name beginning with "Muse". Filtering
// on the Interaxon service UUID would be tidier, but the headband does not
// advertise it — it is only visible after connecting.
func Scan(ctx context.Context, timeout time.Duration) ([]Found, error) {
	if err := enableAdapter(); err != nil {
		return nil, fmt.Errorf("enable bluetooth adapter: %w", err)
	}

	var (
		mu   sync.Mutex
		seen = map[string]Found{}
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Scan blocks until StopScan, so the deadline is enforced from here.
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = adapter.StopScan()
		close(stopped)
	}()

	err := adapter.Scan(func(_ *bluetooth.Adapter, res bluetooth.ScanResult) {
		name := res.LocalName()
		if !strings.HasPrefix(strings.ToLower(name), "muse") {
			return
		}
		mu.Lock()
		seen[res.Address.String()] = Found{
			Name:    name,
			Address: res.Address.String(),
			RSSI:    res.RSSI,
		}
		mu.Unlock()
	})
	cancel()
	<-stopped
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	mu.Lock()
	defer mu.Unlock()
	out := make([]Found, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RSSI > out[j].RSSI })
	return out, nil
}

// Config selects a device and the streams to subscribe to.
type Config struct {
	// Address is a CoreBluetooth peripheral UUID. If empty, Name is used.
	Address string
	// Name matches the advertised local name case-insensitively as a
	// substring. If both Address and Name are empty, the strongest Muse
	// found wins.
	Name string
	// Preset configures which sensors the headband streams, e.g. "p21" or
	// "p50". Empty picks a default from the enabled streams.
	Preset string
	// DisableLight turns off the Muse S indicator LED.
	DisableLight bool
	// Streams to subscribe to. A nil map enables everything.
	Streams map[sample.StreamID]bool
	// ScanTimeout bounds the discovery phase when Address is empty.
	ScanTimeout time.Duration
}

func (c Config) wants(id sample.StreamID) bool {
	if c.Streams == nil {
		return true
	}
	return c.Streams[id]
}

func (c Config) preset() string {
	if c.Preset != "" {
		return c.Preset
	}
	if c.wants(sample.PPG) {
		return PresetWithPPG
	}
	return PresetEEGOnly
}

// Client is a connected headband streaming into a publish callback.
type Client struct {
	dev     bluetooth.Device
	control bluetooth.DeviceCharacteristic
	publish func(sample.Chunk)

	mu       sync.Mutex
	lastData time.Time

	closeOnce sync.Once
}

// Connect finds a headband, subscribes to the configured streams, and starts
// it streaming. publish is called from BLE callback goroutines and must not
// block — send to a hub, do not do I/O in it.
func Connect(ctx context.Context, cfg Config, publish func(sample.Chunk)) (*Client, error) {
	if publish == nil {
		return nil, errors.New("muse: publish callback is required")
	}
	if err := enableAdapter(); err != nil {
		return nil, fmt.Errorf("enable bluetooth adapter: %w", err)
	}

	addr, err := resolveAddress(ctx, cfg)
	if err != nil {
		return nil, err
	}

	dev, err := adapter.Connect(addr, bluetooth.ConnectionParams{})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr.String(), err)
	}

	c := &Client{dev: dev, publish: publish, lastData: time.Now()}
	if err := c.setup(cfg); err != nil {
		_ = dev.Disconnect()
		return nil, err
	}
	return c, nil
}

// resolveAddress turns a Config into a concrete peripheral address, scanning
// only when one was not supplied outright.
func resolveAddress(ctx context.Context, cfg Config) (bluetooth.Address, error) {
	var addr bluetooth.Address

	if cfg.Address != "" {
		// Address.Set swallows parse errors, so parse explicitly: a typo
		// should be an error, not a silent scan for the zero UUID.
		uuid, err := bluetooth.ParseUUID(cfg.Address)
		if err != nil {
			return addr, fmt.Errorf("parse address %q: %w", cfg.Address, err)
		}
		addr.UUID = uuid
		return addr, nil
	}

	timeout := cfg.ScanTimeout
	if timeout <= 0 {
		timeout = time.Duration(ScanTimeoutSeconds * float64(time.Second))
	}
	found, err := Scan(ctx, timeout)
	if err != nil {
		return addr, err
	}
	if len(found) == 0 {
		return addr, errors.New("muse: no headband found (is it powered on and out of its case?)")
	}

	if cfg.Name != "" {
		want := strings.ToLower(cfg.Name)
		for _, f := range found {
			if strings.Contains(strings.ToLower(f.Name), want) {
				return bluetooth.Address{UUID: mustUUID(f.Address)}, nil
			}
		}
		names := make([]string, len(found))
		for i, f := range found {
			names[i] = f.Name
		}
		return addr, fmt.Errorf("muse: no headband matching %q; found %s", cfg.Name, strings.Join(names, ", "))
	}
	return bluetooth.Address{UUID: mustUUID(found[0].Address)}, nil
}

// mustUUID re-parses an address string that came from a scan result, so it is
// known-good by construction.
func mustUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic("muse: scan produced an unparseable address: " + s)
	}
	return u
}

// setup discovers the Interaxon service, wires up notifications, and starts
// the stream. The order matters and mirrors muse-lsl: preset first, then
// subscriptions, then resume.
func (c *Client) setup(cfg Config) error {
	svcUUID, err := bluetooth.ParseUUID(ServiceUUID)
	if err != nil {
		return err
	}
	svcs, err := c.dev.DiscoverServices([]bluetooth.UUID{svcUUID})
	if err != nil {
		return fmt.Errorf("discover muse service: %w", err)
	}
	if len(svcs) == 0 {
		return errors.New("muse: device does not expose the Interaxon service")
	}
	svc := svcs[0]

	chars, err := discoverChars(svc, cfg)
	if err != nil {
		return err
	}

	ctrl, ok := chars[CharControl]
	if !ok {
		return errors.New("muse: control characteristic not found")
	}
	c.control = ctrl

	if err := c.writeCommand(c.preset(cfg)); err != nil {
		return fmt.Errorf("select preset: %w", err)
	}

	if err := c.subscribeStreams(chars, cfg); err != nil {
		return err
	}

	if cfg.DisableLight {
		if err := c.writeString("L0"); err != nil {
			return fmt.Errorf("disable light: %w", err)
		}
	}

	// 'd' resumes streaming. Nothing arrives until this is sent.
	if err := c.writeString("d"); err != nil {
		return fmt.Errorf("start streaming: %w", err)
	}
	return nil
}

// discoverChars looks up every characteristic the config needs, in one round
// trip, and returns them keyed by UUID string.
func discoverChars(svc bluetooth.DeviceService, cfg Config) (map[string]bluetooth.DeviceCharacteristic, error) {
	want := []string{CharControl}
	if cfg.wants(sample.EEG) {
		want = append(want, EEGChannelUUIDs...)
	}
	if cfg.wants(sample.PPG) {
		want = append(want, PPGChannelUUIDs...)
	}
	if cfg.wants(sample.ACC) {
		want = append(want, CharAccelerometer)
	}
	if cfg.wants(sample.Gyro) {
		want = append(want, CharGyro)
	}

	uuids := make([]bluetooth.UUID, 0, len(want))
	for _, s := range want {
		u, err := bluetooth.ParseUUID(s)
		if err != nil {
			return nil, err
		}
		uuids = append(uuids, u)
	}

	found, err := svc.DiscoverCharacteristics(uuids)
	if err != nil {
		return nil, fmt.Errorf("discover characteristics: %w", err)
	}

	out := make(map[string]bluetooth.DeviceCharacteristic, len(found))
	for _, ch := range found {
		out[ch.UUID().String()] = ch
	}
	return out, nil
}

func (c *Client) preset(cfg Config) []byte {
	p := strings.TrimPrefix(cfg.preset(), "p")
	// The preset command is not the usual length-prefixed string form; it is
	// a fixed frame: 0x04, 'p', digits, '\n'.
	cmd := []byte{0x04, 0x70}
	cmd = append(cmd, []byte(p)...)
	return append(cmd, 0x0a)
}

func (c *Client) subscribeStreams(chars map[string]bluetooth.DeviceCharacteristic, cfg Config) error {
	if cfg.wants(sample.EEG) {
		asm := newAssembler(sample.EEG, c.publish, c.touch)
		for i, uuid := range EEGChannelUUIDs {
			ch, ok := chars[uuid]
			if !ok {
				return fmt.Errorf("muse: EEG characteristic %s not found", uuid)
			}
			if err := notifyChannel(ch, i, asm, DecodeEEG); err != nil {
				return err
			}
		}
	}

	if cfg.wants(sample.PPG) {
		asm := newAssembler(sample.PPG, c.publish, c.touch)
		for i, uuid := range PPGChannelUUIDs {
			ch, ok := chars[uuid]
			if !ok {
				return fmt.Errorf("muse: PPG characteristic %s not found (preset may not enable PPG)", uuid)
			}
			if err := notifyChannel(ch, i, asm, DecodePPG); err != nil {
				return err
			}
		}
	}

	if cfg.wants(sample.ACC) {
		if err := c.notifyIMU(chars, CharAccelerometer, sample.ACC, AccelerometerScaleFactor); err != nil {
			return err
		}
	}
	if cfg.wants(sample.Gyro) {
		if err := c.notifyIMU(chars, CharGyro, sample.Gyro, GyroScaleFactor); err != nil {
			return err
		}
	}
	return nil
}

// notifyChannel wires one characteristic of a multi-channel stream into the
// assembler at the given channel index.
func notifyChannel(
	ch bluetooth.DeviceCharacteristic,
	index int,
	asm *assembler,
	decode func([]byte) (uint16, []float64, error),
) error {
	err := ch.EnableNotifications(func(buf []byte) {
		tm, samples, err := decode(buf)
		if err != nil {
			return // a malformed packet is not worth tearing the stream down
		}
		asm.add(tm, index, samples, nowSeconds())
	})
	if err != nil {
		return fmt.Errorf("subscribe to %s: %w", ch.UUID().String(), err)
	}
	return nil
}

// notifyIMU wires up accelerometer or gyroscope. Unlike EEG and PPG these
// arrive whole on a single characteristic, so there is nothing to assemble.
func (c *Client) notifyIMU(
	chars map[string]bluetooth.DeviceCharacteristic,
	uuid string,
	id sample.StreamID,
	scale float64,
) error {
	ch, ok := chars[uuid]
	if !ok {
		return fmt.Errorf("muse: characteristic %s not found", uuid)
	}
	meta := sample.MetaFor(id)
	period := meta.Period()

	err := ch.EnableNotifications(func(buf []byte) {
		_, triples, err := DecodeIMU(buf, scale)
		if err != nil {
			return
		}
		c.touch()

		// muse-lsl stamps IMU from the arrival clock rather than running the
		// RLS fit. We keep that, but spread the packet's samples over their
		// nominal period instead of collapsing them onto one instant, so the
		// series stays strictly increasing.
		arrival := nowSeconds()
		n := len(triples)
		ts := make([]float64, n)
		for i := range ts {
			ts[i] = arrival - float64(n-1-i)*period
		}

		data := make([][]float64, 3)
		for axis := range data {
			col := make([]float64, n)
			for i, t := range triples {
				col[i] = t[axis]
			}
			data[axis] = col
		}
		c.publish(sample.Chunk{Stream: id, Timestamps: ts, Data: data})
	})
	if err != nil {
		return fmt.Errorf("subscribe to %s: %w", uuid, err)
	}
	return nil
}

// Run keeps the connection alive and returns when the context is cancelled or
// the headband stops sending data for AutoDisconnectSeconds.
func (c *Client) Run(ctx context.Context) error {
	keepAlive := time.NewTicker(KeepAliveSeconds * time.Second)
	defer keepAlive.Stop()
	watchdog := time.NewTicker(time.Second)
	defer watchdog.Stop()

	// Give the stream a grace period to produce its first packet before the
	// watchdog is allowed to fire.
	c.touch()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-keepAlive.C:
			if err := c.writeString("k"); err != nil {
				return fmt.Errorf("keep-alive: %w", err)
			}
		case <-watchdog.C:
			c.mu.Lock()
			idle := time.Since(c.lastData)
			c.mu.Unlock()
			if idle > AutoDisconnectSeconds*time.Second {
				return fmt.Errorf("muse: no data for %s, disconnecting", idle.Truncate(time.Millisecond))
			}
		}
	}
}

// touch records that data arrived, feeding the watchdog in Run.
func (c *Client) touch() {
	c.mu.Lock()
	c.lastData = time.Now()
	c.mu.Unlock()
}

// Close halts the stream and disconnects. It is safe to call more than once.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		// Best effort: if the headband already went away, the write fails and
		// there is nothing useful to do about it.
		_ = c.writeString("h")
		err = c.dev.Disconnect()
	})
	return err
}

// writeString sends a command in the headband's length-prefixed form:
// [len+1, chars..., '\n'].
func (c *Client) writeString(s string) error {
	cmd := make([]byte, 0, len(s)+2)
	cmd = append(cmd, byte(len(s)+1))
	cmd = append(cmd, []byte(s)...)
	cmd = append(cmd, '\n')
	return c.writeCommand(cmd)
}

func (c *Client) writeCommand(cmd []byte) error {
	_, err := c.control.WriteWithoutResponse(cmd)
	return err
}

func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}
