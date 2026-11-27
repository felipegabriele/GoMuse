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

`task demo` runs the entire pipeline (hub → filters → wire → browser) off `internal/synth`, so almost all development and manual verification can happen without a headband. Real BLE on macOS requires the terminal to have Bluetooth permission in System Settings > Privacy & Security; on Windows it needs only Bluetooth switched on. The only platform-specific code in this repo is the peripheral address type (`internal/muse/address_darwin.go` holds a CoreBluetooth UUID, `address_hwaddr.go` a MAC for Windows and Linux); everything else, including the BLE calls, goes through `tinygo.org/x/bluetooth`. Check the Windows build from any OS with `task check:windows`.

There are no external test dependencies and no BLE-dependent tests; `task test` passes on any machine.

## Architecture

One producer, a fan-out hub, and N independent consumers:

```
muse.Client (BLE)  ──┐
                     ├─→ hub.Hub ──┬─→ server (WebSocket per browser)
synth.Source (demo) ─┘             └─→ record.Recorder (CSV per stream)
```

- **`internal/sample`** — the shared vocabulary (`StreamID`, `Meta`, `Set`, `Chunk`). It has *no dependencies by design* so that the hub, server and recorder never import the Bluetooth stack. `Chunk.Data` is channel-major: `Data[c][i]`, with one `Timestamps` slice shared across channels. `StreamID` numeric values are part of the WebSocket wire format — do not reorder them. A `Set` is one device's streams: channel counts are a property of the headband, not a constant (`Classic()` has 5 EEG channels and 3-channel PPG, `Athena()` has 4 EEG and a 16-channel `Optics` stream in place of PPG — the two pulse streams are alternatives, and no device produces both), so the server, recorder and wire codec all take a `Set` rather than consulting the package globals.
- **`internal/hub`** — never blocks the producer. The BLE callback stalling would lose samples irrecoverably, so each subscriber has its own buffered channel and full buffers **drop-newest**, counted per subscriber (`Sub.Dropped`) as well as hub-wide. Per-subscriber counts matter: a browser drops routinely by design, a recorder dropping means lost data.
- **`internal/muse`** — BLE connect/scan and two device protocols. Which one a headband speaks is decided by the characteristics it exposes, in `setup`: a Muse S Athena (Gen 3) has only control plus two multiplexed data characteristics, and is handled by `athena.go` (packet framing, tag demux, LSB-first 14-bit EEG, int16 IMU, 20-bit optics — ported from muse-lsl's `athena.py`, itself from BrainFlow PR #779). Note its bit order is the opposite of the classic `decode.go` `bitReader`, it starts with `dc001` rather than `d`, and it needs a control subscription before it will stream at all. Everything else below is the classic path: per-characteristic notification handling, `assemble.go` (rejoins the per-channel EEG/PPG characteristics into whole packets, keyed on channel index rather than muse-lsl's GATT-handle ordering, because CoreBluetooth doesn't expose handles), `decode.go` (MSB-first `bitReader` for the bit-packed 12/14-bit samples), `timestamp.go` (`Dejitter`, an RLS fit of `arrival ≈ offset + slope*index` that smooths bursty BLE arrival into an even time base), `telemetry.go` (the classic battery/fuel-gauge/temperature packet — big-endian uint16 words, the opposite order from the Athena's battery block, and the subscription is deliberately non-fatal and outside the stream flags because losing a battery readout must not cost the session).
- **`internal/wire`** — 16-byte little-endian header + `f32` channel-major payload. `t0` is `f64` and samples are `f32` deliberately; the package comment explains why, and it is not an oversight to be unified.
- **`internal/server`** — embeds `web/` via `//go:embed` (uPlot is vendored, not CDN-loaded, so the binary needs no network). Each WebSocket client has its own hub subscription *and* its own DSP state *and* its own `pulse.Tracker`, because filtering is a per-viewer preference with per-channel history and the tracker likewise carries per-channel history. Since the same `Chunk` is handed to every subscriber, **nothing on the read path may mutate the chunk it receives** — `dsp.Bank.Process`, `dsp.DCBlockerBank.Process` and the whole `pulse` chain return fresh slices, which is what makes this safe. The derived pulse numbers go out as a throttled JSON text message, not as a hub stream: beats are irregularly timed and the `Chunk`/`Dejitter` model assumes a nominal fixed rate. Battery goes out the same way and for a related reason — it is polled from `Config.Battery` rather than decoded from chunks, arrives every five to ten seconds, and is only re-sent when the whole percent changes. A nil `Config.Battery` means "this source has no battery", which `hello` reports so the viewer can omit the indicator; that is distinct from a provider returning `false`, which means "not known yet" and must show as a dash rather than 0%.
- **`internal/dsp`** — a port of muse-lsl's 32-tap 1–40 Hz `firwin` bandpass (verified against scipy to 1e-15 per tap in tests), plus a `DCBlocker` that gomuse adds because that FIR does not actually reject DC. Both files carry measured response tables; keep them accurate if you change the filters.
- **`internal/pulse`** — heart rate and RMSSD HRV from whichever pulse stream the device has, as `Selector` (rank channels by modulation ratio, because which optics channel carries the pulse depends on fit and preset) → `Detector` (DC block, smooth, adaptive-threshold peak pick) → `HRV` (median bpm, RMSSD), assembled by `Tracker`. Two decisions here look wrong at a glance:
  - **Three states, not two.** `no pulse` / `acquiring` / `locked`. `no pulse` and `acquiring` look identical to someone reading bpm, but only one of them resolves by waiting, and the panel must never show a fabricated number. This is why `Tracker` calls `hrv.Reset()` — not `Break()` — on *any* channel transition including to and from -1: keeping pre-loss intervals would let the 7-interval median report the old rate as `locked` for several beats after the headband goes back on, skipping `acquiring` and defeating the whole design.
  - **Two gap thresholds, in absolute seconds.** Data loss is routine, not exceptional: `internal/hub` drops newest, a browser subscriber drops routinely by design, and on an Athena optics tag `0x36` carries one sample per packet, so one dropped chunk is one missing sample. `ChainBreakSec` (50 ms) is where the interval chain breaks — a hole shifts a beat by at most its own length, and one perturbed term among ~70 barely moves RMSSD. `SignalResetSec` (0.5 s) is the much longer hole at which the DC blocker history and peak estimate are thrown away. Welding these together at 1.5 sample periods made the feature unable to lock at all above about one drop per second, because every chain segment was then shorter than one beat period. Both are seconds rather than sample periods because they are about physiology and filter time constants, not rate.
- **`internal/synth`** — waveforms chosen to make viewer features observable (large slow drift on EEG so the filter toggle visibly does something; huge PPG DC pedestal so autoscaling is exercised). Seeded RNG and injectable clock, so tests cover minutes of streaming instantly.
- **`internal/server/web/app.js`** — builds the whole UI from the server's `hello` JSON message; nothing about the streams is hardcoded. Each stream keeps its own x-axis ring buffer (rates differ: 256/64/52/52 Hz), and repainting is throttled to ~30 Hz independent of frame arrival.

## Conventions

- Comments explain *why*, especially where a decision looks wrong at a glance (drop policy, float widths, filter shortcomings, assembly strategy). Match that density; when you change one of these decisions, update the comment that justifies it.
- Tests pin behaviour against golden values and virtual clocks rather than sleeping. Keep new tests hardware-free.
