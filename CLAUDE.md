# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`gomuse` streams EEG/PPG/IMU data from a Muse brain-sensing headband over BLE to a browser viewer and/or CSV. It is a Go port of the device and viewer layers of [muse-lsl](https://github.com/alexandrebarachant/muse-lsl), replacing the matplotlib viewer with an embedded web app and dropping the Lab Streaming Layer dependency. When changing decoding, timestamping or filtering, muse-lsl is the reference implementation — deviations from it are documented in package comments and should stay that way.

## Commands

This project uses [Taskfile](https://taskfile.dev). Use `task` rather than raw `go` invocations — `task --list` shows everything.

```bash
task build                      # build into ./bin/gomuse
task test                       # all tests
task test -- -run TestFirwinBandpass -v   # single test (args after -- go to `go test ./...`)
task test:race                  # tests with the race detector
task test:cover                 # coverage profile + per-function summary
task vet
task lint                       # golangci-lint (see caveat below)
task check                      # full gate: fmt, vet, lint, race tests, build
task clean

task demo                       # viewer with synthetic signals, no headband needed
task list                       # scan for headbands
task stream                     # connect + serve the viewer on :1111
task record -- -dir ./data -duration 60s
task dev                        # watch sources, restart the demo viewer
task run -- <any gomuse args>
```

Extra flags go after `--` (`task stream -- -preset p21`); the port is a Taskfile var (`task stream PORT=8080`).

`task demo` runs the entire pipeline (hub → filters → wire → browser) off `internal/synth`, so almost all development and manual verification can happen without a headband. Real BLE requires the terminal to have Bluetooth permission in macOS System Settings > Privacy & Security.

There are no external test dependencies and no BLE-dependent tests; `task test` passes on any machine.

## Architecture

One producer, a fan-out hub, and N independent consumers:

```
muse.Client (BLE)  ──┐
                     ├─→ hub.Hub ──┬─→ server (WebSocket per browser)
synth.Source (demo) ─┘             └─→ record.Recorder (CSV per stream)
```

- **`internal/sample`** — the shared vocabulary (`StreamID`, `Meta`, `Set`, `Chunk`). It has *no dependencies by design* so that the hub, server and recorder never import the Bluetooth stack. `Chunk.Data` is channel-major: `Data[c][i]`, with one `Timestamps` slice shared across channels. `StreamID` numeric values are part of the WebSocket wire format — do not reorder them. A `Set` is one device's streams: channel counts are a property of the headband, not a constant (`Classic()` has 5 EEG channels and PPG, `Athena()` has 4 and none), so the server, recorder and wire codec all take a `Set` rather than consulting the package globals.
- **`internal/hub`** — never blocks the producer. The BLE callback stalling would lose samples irrecoverably, so each subscriber has its own buffered channel and full buffers **drop-newest**, counted per subscriber (`Sub.Dropped`) as well as hub-wide. Per-subscriber counts matter: a browser drops routinely by design, a recorder dropping means lost data.
- **`internal/muse`** — BLE connect/scan and two device protocols. Which one a headband speaks is decided by the characteristics it exposes, in `setup`: a Muse S Athena (Gen 3) has only control plus two multiplexed data characteristics, and is handled by `athena.go` (packet framing, tag demux, LSB-first 14-bit EEG, int16 IMU, 20-bit optics — ported from muse-lsl's `athena.py`, itself from BrainFlow PR #779). Note its bit order is the opposite of the classic `decode.go` `bitReader`, it starts with `dc001` rather than `d`, and it needs a control subscription before it will stream at all. Everything else below is the classic path: per-characteristic notification handling, `assemble.go` (rejoins the per-channel EEG/PPG characteristics into whole packets, keyed on channel index rather than muse-lsl's GATT-handle ordering, because CoreBluetooth doesn't expose handles), `decode.go` (MSB-first `bitReader` for the bit-packed 12/14-bit samples), `timestamp.go` (`Dejitter`, an RLS fit of `arrival ≈ offset + slope*index` that smooths bursty BLE arrival into an even time base).
- **`internal/wire`** — 16-byte little-endian header + `f32` channel-major payload. `t0` is `f64` and samples are `f32` deliberately; the package comment explains why, and it is not an oversight to be unified.
- **`internal/server`** — embeds `web/` via `//go:embed` (uPlot is vendored, not CDN-loaded, so the binary needs no network). Each WebSocket client has its own hub subscription *and* its own DSP state, because filtering is a per-viewer preference with per-channel history. Since the same `Chunk` is handed to every subscriber, **filters must never mutate the chunk they receive** — `dsp.Bank.Process` and `dsp.DCBlockerBank.Process` return fresh slices, which is what makes this safe.
- **`internal/dsp`** — a port of muse-lsl's 32-tap 1–40 Hz `firwin` bandpass (verified against scipy to 1e-15 per tap in tests), plus a `DCBlocker` that gomuse adds because that FIR does not actually reject DC. Both files carry measured response tables; keep them accurate if you change the filters.
- **`internal/synth`** — waveforms chosen to make viewer features observable (large slow drift on EEG so the filter toggle visibly does something; huge PPG DC pedestal so autoscaling is exercised). Seeded RNG and injectable clock, so tests cover minutes of streaming instantly.
- **`internal/server/web/app.js`** — builds the whole UI from the server's `hello` JSON message; nothing about the streams is hardcoded. Each stream keeps its own x-axis ring buffer (rates differ: 256/64/52/52 Hz), and repainting is throttled to ~30 Hz independent of frame arrival.

## Conventions

- Comments explain *why*, especially where a decision looks wrong at a glance (drop policy, float widths, filter shortcomings, assembly strategy). Match that density; when you change one of these decisions, update the comment that justifies it.
- Tests pin behaviour against golden values and virtual clocks rather than sleeping. Keep new tests hardware-free.
