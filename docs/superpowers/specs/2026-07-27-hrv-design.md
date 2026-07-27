# Pulse and HRV on the dashboard

Date: 2026-07-27
Status: approved, not yet implemented

## Goal

Show live heart rate and RMSSD heart-rate variability in the gomuse viewer,
derived from whichever pulse stream the connected headband produces: the Muse S
Athena's 16-channel `Optics`, or a classic Muse's 3-channel `PPG`.

## Motivation

gomuse plots raw sensor streams and writes CSV; it computes nothing derived. The
cardiac waveform is plainly present in the Athena's optics data — measured on a
MuseS-1FF3 at preset p1041, channels OPT12/13 modulate ~3,800 counts on a ~95,000
pedestal, about 4% AC/DC — but the user has no way to see a pulse rate. HRV in
particular is the metric of interest.

## Scope

In scope:

- Beat detection from the pulse stream.
- Heart rate in bpm.
- RMSSD over a 60 s window.
- A `Pulse` panel showing the selected channel's waveform with beat markers.

Out of scope, deliberately:

- SDNN and pNN50. RMSSD was chosen because it stabilises over 30–60 s and so is
  meaningful on a live dashboard; SDNN needs ~5 minutes and pNN50 is noisy on
  short windows. Showing them would mean showing two numbers that look broken.
- Frequency-domain HRV (LF/HF). Needs minutes of clean, artifact-free data.
- HRV in the CSV recordings. Beats are irregularly timed and the `Chunk` /
  `Dejitter` model assumes a nominal fixed rate; making beats a hub stream would
  mean reworking that model for no present benefit.
- Historical trend charts.

## Architecture

A new `internal/pulse` package, alongside `internal/dsp`. It is held per
WebSocket client, exactly as the existing `dsp.Bank` and `dsp.DCBlockerBank` are,
because it carries per-channel history and is a per-viewer concern.

Placing it in Go rather than in `app.js` follows the existing separation: all
signal processing lives in Go and is pinned by hardware-free tests, and `app.js`
stays a renderer that computes nothing. See CLAUDE.md.

```
hub.Chunk (Optics or PPG)
    │
    ▼
pulse.Selector    rank channels by modulation, pick one
    │
    ▼
pulse.Detector    DC block → smooth → adaptive-threshold peak pick
    │
    ▼  inter-beat intervals
pulse.HRV         median bpm, RMSSD, lock state
    │
    ▼
JSON "pulse" message ──► app.js ──► Pulse panel
```

### pulse.Selector

Ranks channels by modulation ratio `(p95 − p5) / mean` over a rolling 4 s window
(256 samples at 64 Hz), re-evaluated every 2 s.

A channel qualifies only if its modulation ratio exceeds a floor of 0.001
(0.1%). In the reference capture that admits OPT12/13 at ~4% and OPT8/9 at ~2%
while excluding OPT4–7 at ~0.06%. If no channel qualifies, the state is
`no pulse`.

**Channels whose mean is at or below zero are skipped before the ratio is
computed.** `decodeAthenaOptics` zero-fills the canonical channels a tag does not
carry — tag 0x34 fills only slots 4–7 and 0x35 only 0–7 — so on presets other
than the measured p1041 some channels are identically zero. Dividing by their
mean is `0/0`. Go yields NaN there, which happens to lose every `>` comparison
and so would not be selected, but relying on that is accidental; the guard is
explicit. A near-zero mean is excluded for the same reason: it inflates the ratio
without indicating signal.

Channels differ enormously — in the reference capture OPT12/13 modulate ~4% while
OPT4–7 modulate ~0.06% — and which channel is best depends on headband fit and
preset, so a fixed choice is wrong. A challenger replaces the incumbent only if
it beats it by more than 20%, so the selection does not flap between two
near-equal channels every evaluation.

The same logic serves a classic Muse's 3 PPG channels with no special case.

### pulse.Detector

`dsp.DCBlocker` at fc 0.5 Hz removes the pedestal, then a 5-sample moving average
(about 78 ms at 64 Hz) smooths high-frequency noise.

Deliberately **not** `dsp.FirwinBandpass`: at 64 Hz a 0.7 Hz lower edge needs
roughly 512 taps and about 4 s of group delay, which is unusable live. This is
the same limitation `dcblock.go` already documents for EEG, and the same remedy.

Beats are local maxima above an adaptive threshold set to 0.5 of a running peak
estimate, where that estimate decays toward the current signal level with a time
constant of about 3 s so the detector adapts to changing perfusion. A 300 ms
refractory period caps detection at 200 bpm. Intervals outside 300–2000 ms
(30–200 bpm) are rejected as artifacts.

### pulse.HRV

Holds a ring of recent inter-beat intervals.

- **bpm** = `60000 / median` of up to the last 7 intervals — fewer while still
  filling, but never fewer than the 5 required to leave `acquiring`. Median, not
  mean, so a single missed or doubled beat does not move the displayed rate.
- **RMSSD** = root mean square of successive differences over a 60 s window,
  after excluding pairs where successive intervals differ by more than 20% — the
  standard ectopic-beat filter.

### States

Three explicit states, so the panel never shows a fabricated number:

| State       | Meaning                                        | bpm / RMSSD |
| ----------- | ---------------------------------------------- | ----------- |
| `no pulse`  | no channel has enough modulation               | `—` / `—`   |
| `acquiring` | fewer than 5 clean intervals so far            | `—` / `—`   |
| `locked`    | tracking                                       | shown       |

RMSSD additionally stays `—` until about 30 s of intervals have accumulated,
because it is not meaningful before that.

## Data flow and wire format

One new JSON text message, throttled to about 4 Hz:

```json
{"type":"pulse","channel":"OPT12","state":"locked",
 "bpm":68.2,"rmssd":42.1,"ibiMs":880,"beats":[1753649912.41],"beatCount":12}
```

`beats` carries beat timestamps from the last 60 s in Unix seconds, on the same
time base as the `t0` field of the binary frame header. Sixty seconds is
`MAX_WINDOW` in `app.js`, the viewer's longest selectable window; the browser
discards any beats outside what it is currently showing. At a typical rate that
is about 70 timestamps per message, four times a second — negligible next to the
sample frames already on the socket.

**The browser must shift these by `state.epoch` before plotting them.** The
viewer's stored x-values are *not* absolute Unix seconds: `app.js` captures the
first frame's `t0` as `state.epoch` and pushes `frame.t0 - state.epoch`, so the
trace's x-axis is session-relative. An earlier draft of this spec asserted the
opposite, on the strength of reading `tracePush`'s body without checking what
its caller passes for `t0`. Plotting unshifted Unix timestamps against a
session-relative axis puts every marker about 1.7e9 seconds off-screen, so the
panel renders with no beat markers at all and no error.

`bpm` and `rmssd` are `null` in any state other than `locked`, and `rmssd` is
`null` until 30 s of intervals have accumulated. `channel` is `null` in the
`no pulse` state.

`app.js` already parses text frames to find `hello`, so this adds one branch. The
binary chunk format is unchanged.

The message is only sent when the connected device has a pulse stream.

## UI

A `Pulse` stage matching the existing collapsible stages, hidden when the device
has no pulse stream — the same mechanism that hides the PPG or Optics stage.

The browser draws the selected channel from the optics/PPG frames it already
receives. The panel autoscales its y-axis, so the DC pedestal needs no special
handling. Beat markers are overlaid from the server's `beats` timestamps.

Readouts: bpm, RMSSD, selected channel name, and lock state.

## Error handling

- No pulse stream on the device: no message, stage hidden.
- Every channel below the modulation floor: state `no pulse`, no numbers.
- Fewer than 5 clean intervals: state `acquiring`, no numbers.
- Intervals outside 300–2000 ms: dropped, never reach the HRV ring.
- Successive intervals differing by more than 20%: excluded from RMSSD.
- **Gaps in the stream are detected explicitly, not inferred from interval
  length.** The detector compares each chunk's first timestamp against the last
  sample it saw; a jump of more than 1.5 sample periods means samples were lost,
  and the interval spanning that gap is discarded.

  Relying on the 300–2000 ms bound to catch this, as an earlier draft of this
  spec did, is wrong: a gap that swallows one beat turns an 880 ms interval into
  ~1760 ms, which passes the bound.

  A correction to that draft's reasoning, which claimed such an interval would
  double the reported RMSSD: it would not. A merged interval is ~2x its
  neighbours, so the >20% ectopic filter already excludes both differences it
  participates in. The doubled-RMSSD claim was wrong.

  The residual risk is subtler and is the actual justification. A gap shorter
  than one beat does not merge intervals; it shifts a single detected beat time
  by the length of the gap. That produces a pair of small interval errors which
  can sit *below* the 20% ectopic threshold and so pass straight into RMSSD —
  and RMSSD is precisely a measure of small beat-to-beat differences, so
  corrupting it with sub-threshold timing noise is the worst case for this
  metric. Gap detection catches these; the ectopic filter cannot.

  This is not a rare case: `internal/hub` drops newest on a full buffer and a
  browser subscriber drops routinely by design, so gaps are expected rather than
  exceptional. Detecting data loss where it happens is also simply more sound
  than masking it with a downstream heuristic.

## Testing

Hardware-free, per CLAUDE.md.

- `internal/synth` already generates a PPG pulse at `pulseHz = 1.05` (~63 bpm),
  so `task demo` exercises the whole path with no headband. This is the visual
  verification route for the panel.
- Known-BPM synthetic waveform through `Detector` → detected bpm within
  tolerance.
- A fixed interval sequence with a hand-computed RMSSD, pinned as a golden value.
- `Selector`: picks the strongly-modulated channel out of a synthetic set;
  hysteresis prevents flapping between two near-equal channels.
- Injected artifacts (a dropped beat, a doubled beat) → median bpm holds steady
  and the ectopic filter excludes the affected pairs from RMSSD.
- **Gap handling:** assert the *mechanism* rather than a downstream number,
  since the ectopic filter would independently mask a merged interval and make
  an RMSSD assertion pass for the wrong reason. Feed a stream with a timestamp
  discontinuity mid-signal and assert that no interval is formed across it —
  observable as the interval count being one lower than the beat count minus
  one.
- **Zero-filled channels:** a synthetic optics set shaped like tag 0x34 (only
  slots 4–7 carrying, the rest identically zero). Assert the selector picks a
  carrying channel and produces no NaN.
- State transitions: `no pulse` → `acquiring` → `locked` on a synthetic signal
  that starts flat and then begins pulsing.

## Verification

Beyond `task check`, the feature is verified two ways:

1. `task demo` — the panel renders and locks onto the synthetic ~63 bpm pulse.
2. Against the real headband, comparing displayed bpm to a manual pulse count.

Tests passing is not evidence that a headband streams; the same rule the Athena
port followed applies here.
