---
description: Extend Muse S Athena support in gomuse (optics stream, presets, remaining tags)
argument-hint: [optional focus, e.g. "optics stream" or "battery in the viewer"]
---

Continue the Muse S Athena (Gen 3) work in gomuse. Optional focus for this session:
$ARGUMENTS

## What already works — do not redo this

Athena support landed in `internal/muse/athena.go`, ported from muse-lsl's `athena.py`
(itself from BrainFlow PR #779). It is **verified against real hardware**: EEG at
~255 Hz and accelerometer/gyroscope at ~53 Hz stream to the viewer and to CSV, and the
decode is pinned by golden tests whose expected values came from running the Python
reference over the same captured bytes.

- `setup` in `muse.go` branches on `isAthena(present)`, so a headband selects its own
  protocol from the characteristics it exposes.
- `sample.Set` makes stream metadata device-dependent — `sample.Athena()` is 4 EEG
  channels and no PPG; `sample.Classic()` is the old 5-and-3 layout. The server,
  recorder and `wire.Codec` all take a Set.
- Timestamps use `Dejitter.NextArrival`, one fit per stream.

**Three traps already paid for. Do not rediscover them:**

- Streaming starts with `dc001`, not `d` (`d` answers `{"rc":255}`).
- Presets are length-prefixed strings (`p1041`), not the classic `0x04 'p' digits 0x0a`
  frame (which answers `{"rc":69}`). Preset numbers are hex: p1041 reports `"ps":4161`.
- **The headband will not stream unless the client subscribes to the control
  characteristic**, even though nothing reads those replies. Subscribe to control
  before the data characteristics, as `athena.py` does. Without it the whole init
  sequence is accepted, `dc001` answers `{"rc":0}`, and no data ever arrives.

## What is left

1. **Optics** — the big one. `decodeAthenaOptics` is written and unit-tested but the
   result is discarded: 16 channels at 64 Hz with no classic equivalent. Surfacing it
   means a new `StreamID` (**append** — the numeric values are the WebSocket wire
   format), a `Meta` in `sample.Athena()`, and checking `app.js` renders a 16-trace
   stream sanely. Tags `0x34`/`0x35`/`0x36` carry 4/8/16 channels into one canonical
   space via `athenaOpticsIndex`.
2. **Tag `0x53`** — 2 channels at 32 Hz, purpose unknown; muse-lsl labels it `unknown`
   and drops it. Identify it or leave it alone.
3. **Battery** — decoded and available via `Client.Battery()`, surfaced nowhere.
4. **Presets** — only the `p1041` default is exercised. `athenaValidPresets` lists the
   rest; which sensors each enables is not documented anywhere gomuse can see.
5. **The 8-channel EEG tag `0x12`** — channels 4-7 are decoded then dropped. They are
   probably aux/reference; confirm before exposing them.

## Ground rules

- muse-lsl stays the reference (`CLAUDE.md`). The local copy is the spec:
  `~/.local/share/uv/tools/muselsl/lib/python3.13/site-packages/muselsl/athena.py`.
  Cross-check any new decode against it on identical bytes, as `athena_test.go` does —
  that is what makes a golden value trustworthy rather than self-confirming.
- Tests stay hardware-free. Capture real packets with a scratchpad probe and pin
  against them; never require a headband to run `task test`.
- Athena EEG bit order is **LSB-first**, the opposite of the classic `bitReader` in
  `decode.go`. Reusing either for the other produces plausible noise, not an error.
- `task check` must pass, and verify against the real device too — tests passing is not
  evidence that a headband streams.
- The headband stops advertising when asleep or connected. A scan finding nothing means
  ask the user to wake it, not that the scanner is broken.
