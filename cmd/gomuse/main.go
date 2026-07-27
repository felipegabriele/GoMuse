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
	fs.StringVar(&c.address, "address", "", "CoreBluetooth peripheral UUID (see `gomuse list`)")
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

// source starts either a real headband or the synthetic generator, and returns
// a function that runs it until the context is cancelled, along with the stream
// set the source produces.
//
// The stream set comes from the source rather than from a constant because it
// is a property of the hardware: a Muse S Athena has 4 EEG channels and no PPG,
// a classic headband 5 and 3. The viewer and the recorder are both built from
// whatever comes back here.
func (c *connFlags) source(ctx context.Context, h *hub.Hub) (run func(context.Context) error, stop func(), streams *sample.Set, err error) {
	if !c.any() {
		return nil, nil, nil, errors.New("all streams disabled; nothing to do")
	}

	if c.demo {
		fmt.Fprintln(os.Stderr, "running in demo mode with synthetic signals — no headband is being used")
		src := synth.New(h, c.streams())
		return src.Run, func() {}, sample.Classic(), nil
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
		return nil, nil, nil, err
	}
	fmt.Fprintln(os.Stderr, "connected; streaming")
	return client.Run, func() { _ = client.Close() }, client.Streams(), nil
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

	run, stopSource, streams, err := cf.source(ctx, h)
	if err != nil {
		return err
	}
	defer stopSource()

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

	srv := server.New(server.Config{Addr: fmt.Sprintf(":%d", *port), Hub: h, Streams: streams})
	go func() { errs <- srv.ListenAndServe(ctx) }()

	fmt.Fprintf(os.Stderr, "viewer at http://localhost:%d — Ctrl-C to stop\n", *port)

	// The source finishing (auto-disconnect, or the headband going away) ends
	// the run, so give it the same treatment as a server error.
	go func() { errs <- run(ctx) }()

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

	run, stopSource, streams, err := cf.source(ctx, h)
	if err != nil {
		return err
	}
	defer stopSource()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	rec, err := record.New(record.Config{Dir: *dir, Streams: cf.streams(), Meta: streams})
	if err != nil {
		return err
	}

	errs := make(chan error, 2)
	go func() { errs <- run(ctx) }()

	// Run returns when the context is cancelled, having flushed and closed.
	recErr := rec.Run(ctx, h)
	cancel()

	reportRecording(rec, streams)

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
