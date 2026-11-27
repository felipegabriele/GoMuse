# gomuse on Windows

gomuse runs on Windows 10 and 11 with the same commands, flags and CSV output
as on macOS. This page covers setup, the few differences, and a checklist for
confirming a build against a real headband.

## Setup

1. Install [Go 1.24 or newer](https://go.dev/dl/). No C compiler is needed.
2. Turn Bluetooth on in **Settings → Bluetooth & devices**.
3. Build and install, from PowerShell:

   ```powershell
   git clone https://github.com/felipegabriele/GoMuse.git
   cd GoMuse
   go install ./cmd/gomuse
   ```

   Make sure `%USERPROFILE%\go\bin` is on your `PATH`. To get a standalone
   `gomuse.exe` instead, run `go build -o gomuse.exe ./cmd/gomuse`. It can also
   be cross-compiled from another system with `task build:windows`.
4. Try it without a headband first:

   ```powershell
   gomuse stream -demo
   ```

   Windows Defender Firewall may ask whether to let `gomuse.exe` accept network
   connections, because the viewer listens on a TCP port. For local use only,
   allow private networks and nothing else. The viewer has no authentication.

## Differences from macOS

| | macOS | Windows |
| --- | --- | --- |
| Bluetooth API | CoreBluetooth | WinRT |
| Permission | Terminal needs Bluetooth permission | None |
| Peripheral address | Per-host UUID | Bluetooth MAC address, portable |
| `-address` accepts | UUID | `AA:BB:CC:DD:EE:FF`, `AA-BB-CC-DD-EE-FF`, `AABBCCDDEEFF` |
| Stopping a recording | `Ctrl-C` flushes the CSV files | `Ctrl-C` flushes the CSV files |

Everything else, including packet decoding, timestamping, filtering, the viewer
and the CSV writer, is shared code and identical on both systems.

One Windows limitation to know about when driving gomuse from another program:
a parent cannot stop `gomuse record` by sending it an interrupt the way it
could with `SIGINT` on macOS (Go does not implement that signal for processes on
Windows), so asking it to stop and flush needs another mechanism. Use
`-duration` for fixed-length recordings, or run it in a console and press
`Ctrl-C`.

## Hardware test checklist

The Windows build compiles and vets cleanly, and the hardware-free unit tests
pass on Linux, where the address handling shares its code with Windows. The
tests have not been run on Windows itself. What follows needs a real headband
and a Windows computer, and has not been run. None of these steps need the headband to be worn: connecting and
streaming work with the headband on a table, and the signal will simply be noise.

- [ ] `gomuse stream -demo` opens the viewer at `http://localhost:1111` and
      shows moving traces.
- [ ] `gomuse list` shows the headband by name with a MAC address and RSSI.
- [ ] `gomuse list` shows the same headband on a second run (the scan stops and
      restarts cleanly).
- [ ] `gomuse record -dir .\data -duration 30s` writes `eeg_*`, `ppg_*`, `acc_*`
      and `gyro_*` files for a classic Muse S, and the final rows have a
      timestamp close to the computer's clock.
- [ ] The row counts match the nominal rates (EEG 256 Hz, PPG 64 Hz,
      accelerometer and gyroscope 52 Hz) to within a few percent over 30 s.
- [ ] `Ctrl-C` during `gomuse record` ends the run and the last CSV row is
      complete.
- [ ] `gomuse record -stdin-stop -manifest`, then typing `stop` and Enter, ends
      the run, and `manifest.json` lists the files with `"stop_reason":
      "stdin-stop"`. Compare one hash with `Get-FileHash`.
- [ ] `gomuse stream -address <MAC from list>` connects without scanning.
- [ ] Unplug nothing, but switch the headband off during `stream`: gomuse
      reports the loss and exits instead of hanging.
- [ ] Run `gomuse stream` and `gomuse record` twice in a row without restarting
      Windows, to confirm the previous connection is released.
- [ ] Optional, needs the headband worn: signal quality on AF7 and AF8 in the
      viewer.

If a connection fails with "could not retrieve device services", move the
headband closer, make sure no other program (a phone app, another computer) holds
the connection, and try again. If that persists, check whether the headband is
listed under **Settings → Bluetooth & devices**; removing it from that list is
worth trying, but is not confirmed to be needed.
