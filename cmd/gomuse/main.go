// Command gomuse streams EEG, PPG and IMU data from a Muse headband to a
// browser and to CSV.
//
// It is a Go port of the device and viewer layers of muse-lsl
// (https://github.com/alexandrebarachant/muse-lsl), replacing the matplotlib
// viewer with a web server and dropping the Lab Streaming Layer dependency.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gomuse/internal/hub"
	"gomuse/internal/muse"
	"gomuse/internal/record"
	"gomuse/internal/sample"
	"gomuse/internal/server"
	"gomuse/internal/synth"
)

const usage = `gomuse — stream a Muse headband to your browser

Usage:
  gomuse list                 Scan for nearby Muse headbands
  gomuse stream [flags]       Connect and serve the live viewer
  gomuse record [flags]       Connect and record to CSV

Run "gomuse <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	// Ctrl-C cancels the context; a second one kills the process outright, in
	// case a BLE call is wedged.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "list":
		err = runList(ctx, os.Args[2:])
	case "stream":
		err = runStream(ctx, os.Args[2:])
	case "record":
		err = runRecord(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "gomuse: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "gomuse: %v\n", err)
		os.Exit(1)
	}
}

// connFlags are the flags shared by stream and record.
type connFlags struct {
	address      string
	name         string
	preset       string
	disableLight bool
	scanTimeout  time.Duration
	demo         bool

	eeg, ppg, acc, gyro, optics bool
}

func (c *connFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.address, "address", "", "peripheral address: "+muse.AddressHelp+" (see `gomuse list`)")
	fs.StringVar(&c.name, "name", "", "match the headband by name, e.g. MuseS-4A2B")
	fs.StringVar(&c.preset, "preset", "", "headband preset (default p50 with PPG, p21 without)")
	fs.BoolVar(&c.disableLight, "disable-light", false, "turn off the indicator LED (Muse S)")
	fs.DurationVar(&c.scanTimeout, "scan-timeout", 10*time.Second, "how long to scan when no address is given")
	fs.BoolVar(&c.demo, "demo", false, "generate synthetic signals instead of connecting to a headband")

	fs.BoolVar(&c.eeg, "eeg", true, "stream EEG")
	fs.BoolVar(&c.ppg, "ppg", true, "stream PPG (classic headbands; the Athena's equivalent is -optics)")
	fs.BoolVar(&c.acc, "acc", true, "stream accelerometer")
	fs.BoolVar(&c.gyro, "gyro", true, "stream gyroscope")
	fs.BoolVar(&c.optics, "optics", true, "stream 16-channel optics (Muse S Athena only)")
}

func (c *connFlags) streams() map[sample.StreamID]bool {
	// Every stream any supported headband produces must appear here, including
	// those a given device lacks: muse.Config.wants reads this map directly, so
	// an omitted key reads as "disabled" rather than "not applicable".
	return map[sample.StreamID]bool{
		sample.EEG:    c.eeg,
		sample.PPG:    c.ppg,
		sample.ACC:    c.acc,
		sample.Gyro:   c.gyro,
		sample.Optics: c.optics,
	}
}

func (c *connFlags) any() bool { return c.eeg || c.ppg || c.acc || c.gyro || c.optics }

// source is a started data source: everything the rest of the program needs
// from either a real headband or the synthetic generator.
//
// streams comes from the source rather than from a constant because it is a
// property of the hardware: a Muse S Athena has 4 EEG channels and no PPG, a
// classic headband 5 and 3. The viewer and the recorder are both built from
// whatever comes back here.
type source struct {
	// run publishes until the context is cancelled.
	run func(context.Context) error
	// stop releases the source. Always non-nil, so callers can defer it.
	stop func()
	// streams describes what this source produces.
	streams *sample.Set
	// battery reports the charge level and whether it is known yet. Never nil
	// here, but the server treats nil as "this source has no battery".
	battery func() (float64, bool)
}

// start brings up either a real headband or the synthetic generator.
func (c *connFlags) start(ctx context.Context, h *hub.Hub) (*source, error) {
	if !c.any() {
		return nil, errors.New("all streams disabled; nothing to do")
	}

	if c.demo {
		src := synth.New(h, c.streams())
		// synth builds a generator only for the streams in sample.All(), which
		// deliberately omits Optics — there is no synthetic optics waveform.
		// So -optics alone satisfies any() (it is a legitimate configuration
		// for a real Athena, whose pulse sensor *is* the optics stream) while
		// leaving the demo with nothing to publish: Source.Run would park on
		// <-ctx.Done() and record would only admit "no data was recorded"
		// after the duration elapsed — never, for an unbounded run — while
		// stream served an empty viewer forever. Ask the source how many
		// generators it actually built rather than re-listing IDs here, so
		// this check cannot drift from what synth iterates.
		if src.Streams() == 0 {
			// Name the supported streams from the same list synth walks, for
			// the same reason.
			names := make([]string, 0, len(sample.All()))
			for _, id := range sample.All() {
				names = append(names, sample.MetaFor(id).Name)
			}
			return nil, fmt.Errorf("demo mode has no generator for any enabled stream (there is no synthetic optics signal); enable at least one of: %s", strings.Join(names, ", "))
		}
		fmt.Fprintln(os.Stderr, "running in demo mode with synthetic signals — no headband is being used")
		return &source{
			run:     src.Run,
			stop:    func() {},
			streams: sample.Classic(),
			battery: src.Battery,
		}, nil
	}

	cfg := muse.Config{
		Address:      c.address,
		Name:         c.name,
		Preset:       c.preset,
		DisableLight: c.disableLight,
		Streams:      c.streams(),
		ScanTimeout:  c.scanTimeout,
		Warn:         func(msg string) { fmt.Fprintln(os.Stderr, "warning: "+msg) },
	}

	if c.address == "" {
		fmt.Fprintln(os.Stderr, "scanning for a headband…")
	}
	client, err := muse.Connect(ctx, cfg, h.Publish)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "connected; streaming")
	return &source{
		run:     client.Run,
		stop:    func() { _ = client.Close() },
		streams: client.Streams(),
		battery: client.Battery,
	}, nil
}

func runList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	timeout := fs.Duration("timeout", 10*time.Second, "how long to scan")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "scanning for %s…\n", *timeout)
	found, err := muse.Scan(ctx, *timeout)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return errors.New("no headband found — check it is powered on, out of its case, and that this terminal has Bluetooth permission in System Settings > Privacy & Security")
	}
	for _, f := range found {
		fmt.Printf("%-16s %s  (%d dBm)\n", f.Name, f.Address, f.RSSI)
	}
	return nil
}

func runStream(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	var cf connFlags
	cf.register(fs)
	port := fs.Int("port", 1111, "port for the web viewer")
	recordDir := fs.String("record-dir", "", "also record to CSV in this directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	h := hub.New()
	defer h.Close()

	src, err := cf.start(ctx, h)
	if err != nil {
		return err
	}
	defer src.stop()
	streams := src.streams

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, 3)

	var rec *record.Recorder
	if *recordDir != "" {
		rec, err = record.New(record.Config{Dir: *recordDir, Streams: cf.streams(), Meta: streams})
		if err != nil {
			return err
		}
		go func() { errs <- rec.Run(ctx, h) }()
	}

	srv := server.New(server.Config{
		Addr:    fmt.Sprintf(":%d", *port),
		Hub:     h,
		Streams: streams,
		Battery: src.battery,
	})
	go func() { errs <- srv.ListenAndServe(ctx) }()

	fmt.Fprintf(os.Stderr, "viewer at http://localhost:%d — Ctrl-C to stop\n", *port)

	// The source finishing (auto-disconnect, or the headband going away) ends
	// the run, so give it the same treatment as a server error.
	go func() { errs <- src.run(ctx) }()

	err = <-errs
	cancel()
	if rec != nil {
		reportRecording(rec, streams)
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func runRecord(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	var cf connFlags
	cf.register(fs)
	dir := fs.String("dir", ".", "directory for the CSV files")
	duration := fs.Duration("duration", 0, "stop after this long (0 means run until Ctrl-C)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	h := hub.New()
	defer h.Close()

	src, err := cf.start(ctx, h)
	if err != nil {
		return err
	}
	defer src.stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	rec, err := record.New(record.Config{Dir: *dir, Streams: cf.streams(), Meta: src.streams})
	if err != nil {
		return err
	}

	errs := make(chan error, 2)
	go func() { errs <- src.run(ctx) }()

	// Run returns when the context is cancelled, having flushed and closed.
	recErr := rec.Run(ctx, h)
	cancel()

	reportRecording(rec, src.streams)

	if recErr != nil && !errors.Is(recErr, context.Canceled) {
		return recErr
	}
	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	default:
	}
	return nil
}

// reportRecording prints what was written, and warns loudly if rows were lost.
func reportRecording(rec *record.Recorder, streams *sample.Set) {
	files := rec.Files()
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "no data was recorded")
		return
	}
	rows := rec.Rows()
	fmt.Fprintln(os.Stderr, "wrote:")
	for _, f := range files {
		var n int
		for id, count := range rows {
			if strings.Contains(f, streams.MetaFor(id).Name+"_") {
				n = count
				break
			}
		}
		fmt.Fprintf(os.Stderr, "  %s (%d rows)\n", f, n)
	}
	if d := rec.Dropped(); d > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d chunks were dropped — this recording has gaps\n", d)
	}
	if m := rec.Malformed(); m > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d malformed chunks were skipped\n", m)
	}
}
