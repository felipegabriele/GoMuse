/* gomuse viewer.
 *
 * Reads binary sample frames off the WebSocket, keeps a per-stream ring buffer
 * of the visible window, and repaints uPlot charts on a throttled
 * requestAnimationFrame loop.
 *
 * Three things are deliberate:
 *
 *  - Nothing about the streams is hardcoded. The server sends a `hello` JSON
 *    message describing every stream (id, channel names, unit, rate), and the
 *    whole UI is built from it.
 *  - Each stream has its own timestamp axis. The four streams run at 256, 64,
 *    52 and 52 Hz, so a shared sample index would be wrong; every ring buffer
 *    stores its own x values.
 *  - Repainting is decoupled from arrival. EEG frames land ~21 times a second
 *    and the other streams at their own cadence; drawing is capped at ~30 Hz
 *    regardless, so a burst of packets cannot turn into a burst of layouts.
 */
(function () {
  'use strict';

  // ------------------------------------------------------------- protocol

  // Frame layout, little-endian (see internal/wire/wire.go):
  //   0  u8   stream id
  //   1  u8   nchan
  //   2  u16  nsamples
  //   4  f64  t0, Unix seconds of the first sample
  //   12 f32  dt, sample period in seconds
  //   16 f32[nchan * nsamples], channel-major
  var HEADER_SIZE = 16;

  function decodeFrame(buffer) {
    if (buffer.byteLength < HEADER_SIZE) return null;
    var dv = new DataView(buffer);
    var nchan = dv.getUint8(1);
    var nsamples = dv.getUint16(2, true);
    if (buffer.byteLength !== HEADER_SIZE + 4 * nchan * nsamples) return null;

    var chans = new Array(nchan);
    var off = HEADER_SIZE;
    for (var c = 0; c < nchan; c++) {
      var col = new Float64Array(nsamples);
      for (var i = 0; i < nsamples; i++) {
        col[i] = dv.getFloat32(off, true);
        off += 4;
      }
      chans[c] = col;
    }
    return {
      stream: dv.getUint8(0),
      t0: dv.getFloat64(4, true),
      dt: dv.getFloat32(12, true),
      nsamples: nsamples,
      chans: chans
    };
  }

  // ---------------------------------------------------------------- state

  var MIN_WINDOW = 1;
  var MAX_WINDOW = 60;
  var MIN_SCALE = 1;
  var MAX_SCALE = 100000;
  var DRAW_INTERVAL = 1000 / 30;
  var STATS_INTERVAL = 250;

  var state = {
    window: 5,      // seconds of history on screen
    scale: 100,     // EEG half-height, in the stream's unit
    filter: false,
    epoch: null,    // Unix seconds of the first sample of the session
    built: false
  };

  var streams = [];        // in server order, index by array position
  var streamsById = {};    // stream id -> stream object
  var ws = null;

  // Latest derived pulse message. The browser computes none of this — see the
  // internal/pulse package.
  var pulse = { state: 'no pulse', bpm: null, rmssd: null, ibiMs: null, channel: null, beats: [] };
  var backoff = 500;
  var framesThisSecond = 0;
  var rateStamp = 0;

  var el = {
    link: document.getElementById('link'),
    linkText: document.getElementById('linkText'),
    linkRate: document.getElementById('linkRate'),
    scaleValue: document.getElementById('scaleValue'),
    windowValue: document.getElementById('windowValue'),
    filterToggle: document.getElementById('filterToggle'),
    filterLabel: document.getElementById('filterLabel'),
    eegChannels: document.getElementById('eegChannels'),
    eegRate: document.getElementById('eegRate'),
    pulseReadout: document.querySelector('.pulse-readout'),
    pulseBpm: document.getElementById('pulseBpm'),
    pulseRmssd: document.getElementById('pulseRmssd'),
    pulseIbi: document.getElementById('pulseIbi'),
    pulseState: document.getElementById('pulseState'),
    pulseChannel: document.getElementById('pulseChannel')
  };

  // ---------------------------------------------------------- ring buffer

  // A trace is a fixed-capacity, append-only buffer of the recent past. When
  // it fills, the oldest samples are dropped in one block move rather than one
  // per sample, so appending stays amortised O(1).
  function makeTrace(nchan, target) {
    var cap = target + 1024;
    var y = new Array(nchan);
    for (var c = 0; c < nchan; c++) y[c] = new Float64Array(cap);
    return { nchan: nchan, target: target, cap: cap, len: 0, x: new Float64Array(cap), y: y };
  }

  function tracePush(tr, t0, dt, chans) {
    var n = chans[0].length;
    if (n <= 0 || n > tr.cap) return;

    if (tr.len + n > tr.cap) {
      var keep = Math.max(Math.min(tr.len, tr.target - n), 0);
      var from = tr.len - keep;
      tr.x.copyWithin(0, from, tr.len);
      for (var c = 0; c < tr.nchan; c++) tr.y[c].copyWithin(0, from, tr.len);
      tr.len = keep;
    }

    var base = tr.len;
    for (var i = 0; i < n; i++) tr.x[base + i] = t0 + i * dt;
    var m = Math.min(tr.nchan, chans.length);
    for (var ch = 0; ch < m; ch++) {
      var dst = tr.y[ch], src = chans[ch];
      for (var j = 0; j < n; j++) dst[base + j] = src[j];
    }
    tr.len = base + n;
  }

  // traceResize keeps the newest samples that still fit the new window.
  function traceResize(tr, target) {
    if (target === tr.target) return tr;
    var next = makeTrace(tr.nchan, target);
    var keep = Math.min(tr.len, target);
    var from = tr.len - keep;
    next.x.set(tr.x.subarray(from, tr.len));
    for (var c = 0; c < tr.nchan; c++) next.y[c].set(tr.y[c].subarray(from, tr.len));
    next.len = keep;
    return next;
  }

  function stdDev(values, len) {
    if (len < 2) return 0;
    var sum = 0, i;
    for (i = 0; i < len; i++) sum += values[i];
    var mean = sum / len;
    var acc = 0;
    for (i = 0; i < len; i++) {
      var d = values[i] - mean;
      acc += d * d;
    }
    return Math.sqrt(acc / (len - 1));
  }

  function meanOf(values, len) {
    if (len === 0) return 0;
    var sum = 0;
    for (var i = 0; i < len; i++) sum += values[i];
    return sum / len;
  }

  // --------------------------------------------------------------- colors

  function colors() {
    var cs = getComputedStyle(document.body);
    function v(name) { return cs.getPropertyValue(name).trim(); }
    return {
      text: v('--text-secondary'),
      muted: v('--text-muted'),
      grid: v('--grid'),
      series: [v('--series-1'), v('--series-2'), v('--series-3')]
    };
  }

  // n is the stream's channel count. A wide stream outruns the three-colour
  // palette — Athena optics is 16 channels, which would give five of them the
  // same stroke — so those get evenly spaced hues instead, keeping every trace
  // in the panel distinguishable. Narrow streams keep the designed palette.
  function seriesColor(c, i, n) {
    if (!n || n <= c.series.length) return c.series[i % c.series.length];
    return 'hsl(' + Math.round((360 / n) * i) + ', 70%, 58%)';
  }

  // WIDE_STREAM is the channel count above which a panel switches to generated
  // hues and hides its silent channels. Only optics reaches it today.
  var WIDE_STREAM = 8;

  // --------------------------------------------------------------- charts

  function axisFont() { return '10px ui-monospace, SFMono-Regular, Menlo, monospace'; }

  function xAxis(c, show) {
    return {
      show: show,
      stroke: c.muted,
      font: axisFont(),
      size: show ? 26 : 0,
      gap: 3,
      grid: { show: show, stroke: c.grid, width: 1 },
      ticks: { show: show, stroke: c.grid, size: 3 },
      // Keep ticks well apart: at the default density a 5 s window produces a
      // tick every 0.25 s, and rounding those to whole seconds prints
      // "-4s -4s -4s -4s -3s ..." across the axis.
      space: 70,
      // Label relative to the newest sample, so the axis reads as "how long
      // ago" (-5s .. 0s) the way muse-lsl's viewer does. The underlying x
      // values are seconds since the stream started, which would otherwise
      // show as an ever-growing absolute count.
      //
      // Decimals come from the tick spacing uPlot actually chose, so a 1 s
      // window labels tenths while a 30 s window stays on whole seconds.
      values: function (u, splits, axisIdx, foundSpace, foundIncr) {
        var dec = foundIncr && foundIncr < 1
          ? Math.min(2, Math.ceil(-Math.log10(foundIncr)))
          : 0;
        var end = u.scales.x.max;
        return splits.map(function (s) { return (s - end).toFixed(dec) + 's'; });
      }
    };
  }

  function yAxis(c, size) {
    return {
      scale: 'y',
      stroke: c.muted,
      font: axisFont(),
      size: size,
      gap: 3,
      grid: { stroke: c.grid, width: 1 },
      ticks: { stroke: c.grid, size: 3 },
      values: function (u, splits) { return splits.map(compact); }
    };
  }

  // compact keeps PPG's ~1.6e6 raw counts from overrunning the axis gutter.
  function compact(v) {
    var abs = Math.abs(v);
    if (abs >= 1e6) return (v / 1e6).toFixed(abs >= 1e7 ? 0 : 1) + 'M';
    if (abs >= 1e4) return (v / 1e3).toFixed(0) + 'k';
    if (abs >= 10) return v.toFixed(0);
    if (abs >= 1) return v.toFixed(1);
    if (abs === 0) return '0';
    return v.toFixed(2);
  }

  function chartWidth(node) {
    return Math.max(120, Math.floor(node.clientWidth) - 2);
  }

  // buildEEG lays out one chart per electrode, each with its own rail.
  function buildEEG(stream) {
    var c = colors();
    el.eegChannels.textContent = '';
    stream.charts = [];
    stream.readouts = [];
    stream.center = new Float64Array(stream.meta.channels.length);

    stream.meta.channels.forEach(function (name, idx) {
      var row = document.createElement('div');
      row.className = 'channel';

      var rail = document.createElement('div');
      rail.className = 'rail';
      rail.innerHTML =
        '<span class="electrode"></span>' +
        '<span class="sigma mono">σ —</span>' +
        '<span class="quality" data-level="good">' +
        '<span class="quality-bar"><span class="quality-fill"></span></span>' +
        '<span>—</span></span>';
      rail.querySelector('.electrode').textContent = name;

      var plot = document.createElement('div');
      plot.className = 'plot';

      row.appendChild(rail);
      row.appendChild(plot);
      el.eegChannels.appendChild(row);

      var last = idx === stream.meta.channels.length - 1;
      var u = new uPlot({
        width: chartWidth(plot),
        height: last ? 104 : 84,
        padding: [8, 10, 0, 0],
        legend: { show: false },
        cursor: {
          y: false,
          points: { show: false },
          drag: { x: false, y: false, setScale: false }
        },
        // Both scales are driven explicitly from the draw loop: x by the
        // window, y by the channel's own mean plus the amplitude setting, so
        // an unfiltered trace riding a 300 uV drift stays on screen.
        scales: { x: { time: false, auto: false }, y: { auto: false } },
        axes: [xAxis(c, last), yAxis(c, 46)],
        series: [
          {},
          { stroke: seriesColor(c, 0), width: 1.25, points: { show: false } }
        ]
      }, [new Float64Array(0), new Float64Array(0)], plot);

      stream.charts.push(u);
      stream.readouts.push({
        sigma: rail.querySelector('.sigma'),
        quality: rail.querySelector('.quality'),
        fill: rail.querySelector('.quality-fill'),
        word: rail.querySelector('.quality span:last-child'),
        node: plot
      });
    });
  }

  // buildPanel lays out one overlaid chart for a multi-channel stream.
  function buildPanel(stream, panel) {
    var c = colors();
    panel.textContent = '';

    var plot = document.createElement('div');
    plot.className = 'plot';
    panel.appendChild(plot);

    var stats = document.createElement('div');
    stats.className = 'panel-stats';
    panel.appendChild(stats);

    var nchan = stream.meta.channels.length;
    var series = [{}];
    stream.meta.channels.forEach(function (name, i) {
      series.push({
        label: name,
        stroke: seriesColor(c, i, nchan),
        width: 1.25,
        points: { show: false }
      });
    });

    var data = [new Float64Array(0)];
    stream.meta.channels.forEach(function () { data.push(new Float64Array(0)); });

    var u = new uPlot({
      width: chartWidth(plot),
      height: 168,
      padding: [8, 10, 0, 0],
      legend: { show: true, live: true },
      cursor: { y: false, drag: { x: false, y: false, setScale: false } },
      scales: { x: { time: false, auto: false }, y: { auto: true } },
      axes: [xAxis(c, true), yAxis(c, 62)],
      series: series
    }, data, plot);

    stream.charts = [u];
    stream.readouts = stream.meta.channels.map(function (name, i) {
      var span = document.createElement('span');
      span.className = 'stat';
      span.innerHTML = '<i></i><b></b><em class="mono">σ —</em>';
      span.querySelector('i').style.background = seriesColor(c, i, nchan);
      span.querySelector('b').textContent = name;
      stats.appendChild(span);
      return { sigma: span.querySelector('em'), node: plot, chip: span };
    });
    // Tracks which channels are currently hidden as silent, so the series are
    // only toggled when that changes rather than on every stats pass.
    stream.silent = stream.meta.channels.map(function () { return false; });
  }

  // applyPulse stores the server's derived numbers and repaints the readout.
  // A null bpm or rmssd means "not known yet", which is why the wire uses null
  // rather than 0 — see PulseMessage in server.go.
  function applyPulse(msg) {
    pulse.state = msg.state || 'no pulse';
    pulse.bpm = typeof msg.bpm === 'number' ? msg.bpm : null;
    pulse.rmssd = typeof msg.rmssd === 'number' ? msg.rmssd : null;
    pulse.ibiMs = typeof msg.ibiMs === 'number' ? msg.ibiMs : null;
    pulse.channel = msg.channel || null;
    pulse.beats = msg.beats || [];

    if (el.pulseReadout) el.pulseReadout.dataset.state = pulse.state;
    if (el.pulseBpm) el.pulseBpm.textContent = pulse.bpm === null ? '—' : pulse.bpm.toFixed(0);
    if (el.pulseRmssd) el.pulseRmssd.textContent = pulse.rmssd === null ? '—' : pulse.rmssd.toFixed(0);
    // The last inter-beat interval, the raw quantity bpm and RMSSD are both
    // derived from. Shown to the millisecond because that is the resolution the
    // variability being reported lives at.
    if (el.pulseIbi) el.pulseIbi.textContent = pulse.ibiMs === null ? '—' : pulse.ibiMs.toFixed(0);
    if (el.pulseState) el.pulseState.textContent = pulse.state;
    if (el.pulseChannel) el.pulseChannel.textContent = pulse.channel || '';

    var s = pulseStream();
    if (s) s.dirty = true;
  }

  // pulseStream returns the stream the Pulse panel draws: optics on an Athena,
  // PPG on a classic headband. They are alternatives; no device has both.
  function pulseStream() {
    for (var i = 0; i < streams.length; i++) {
      if (streams[i].meta.name === 'optics' || streams[i].meta.name === 'ppg') return streams[i];
    }
    return null;
  }

  // buildPulse lays out the Pulse panel: one trace for the selected channel,
  // plus a points-only series carrying the detected beats.
  function buildPulse(stream, panel) {
    var c = colors();
    panel.textContent = '';

    var plot = document.createElement('div');
    plot.className = 'plot';
    panel.appendChild(plot);

    var u = new uPlot({
      width: chartWidth(plot),
      height: 168,
      padding: [8, 10, 0, 0],
      legend: { show: false },
      cursor: { y: false, drag: { x: false, y: false, setScale: false } },
      scales: { x: { time: false, auto: false }, y: { auto: true } },
      axes: [xAxis(c, true), yAxis(c, 62)],
      series: [
        {},
        { stroke: seriesColor(c, 0), width: 1.25, points: { show: false } },
        // Beat markers. Drawn as points on a series that is NaN everywhere
        // except at a detected beat, so uPlot spaces them for us instead of
        // needing a custom draw hook.
        {
          stroke: seriesColor(c, 1),
          width: 0,
          points: { show: true, size: 7, fill: seriesColor(c, 1) }
        }
      ]
    }, [new Float64Array(0), new Float64Array(0), new Float64Array(0)], plot);

    stream.pulseChart = u;
  }

  // beatSeries maps beat timestamps onto the trace's x grid, returning a series
  // that is NaN except at the sample nearest each beat. beats arrive from the
  // server as absolute Unix seconds (same base as the frame header's t0), but
  // xs is relative to state.epoch like every other trace, so callers must
  // shift beats by the epoch before passing them in here.
  function beatSeries(xs, len, ys, beats) {
    var out = new Float64Array(len);
    for (var i = 0; i < len; i++) out[i] = NaN;
    if (!beats || !len) return out;

    var j = 0;
    for (var b = 0; b < beats.length; b++) {
      var t = beats[b];
      if (t < xs[0] || t > xs[len - 1]) continue;
      while (j < len - 1 && xs[j + 1] <= t) j++;
      // Snap to whichever neighbour is closer.
      var k = (j + 1 < len && Math.abs(xs[j + 1] - t) < Math.abs(xs[j] - t)) ? j + 1 : j;
      out[k] = ys[k];
    }
    return out;
  }

  var PANELS = {
    ppg: 'panel-ppg',
    optics: 'panel-optics',
    acc: 'panel-acc',
    gyro: 'panel-gyro'
  };

  // Stages a headband may not have. PPG and optics are alternatives — a classic
  // Muse sends the first, an Athena the second — so the one that did not appear
  // in hello is hidden rather than left as an empty panel that looks broken.
  var OPTIONAL_STAGES = { ppg: 'stage-ppg', optics: 'stage-optics' };

  // Stages derived from a stream rather than named by one. Pulse is shown when
  // *either* pulse sensor is present, since it is computed from whichever one
  // the device has. It belongs here and not in the per-stream build loop
  // because only showStages sets hidden in both directions: revealed ad hoc, a
  // pulse stage stayed visible after reconnecting to a device without one.
  var DERIVED_STAGES = { 'stage-pulse': ['ppg', 'optics'] };

  function showStages(hello) {
    var present = {};
    hello.streams.forEach(function (m) { present[m.name] = true; });
    Object.keys(OPTIONAL_STAGES).forEach(function (name) {
      var stage = document.getElementById(OPTIONAL_STAGES[name]);
      if (stage) stage.hidden = !present[name];
    });
    Object.keys(DERIVED_STAGES).forEach(function (id) {
      var stage = document.getElementById(id);
      if (stage) {
        stage.hidden = !DERIVED_STAGES[id].some(function (n) { return present[n]; });
      }
    });
  }

  function build(hello) {
    streams = [];
    streamsById = {};
    showStages(hello);

    hello.streams.forEach(function (meta) {
      var target = Math.ceil(state.window * meta.rate);
      var stream = {
        meta: meta,
        trace: makeTrace(meta.channels.length, target),
        charts: [],
        readouts: [],
        center: new Float64Array(meta.channels.length),
        dirty: false
      };
      streams.push(stream);
      streamsById[meta.id] = stream;

      if (meta.name === 'eeg') {
        buildEEG(stream);
        if (el.eegRate) el.eegRate.textContent = String(meta.rate);
      } else if (PANELS[meta.name]) {
        var panel = document.getElementById(PANELS[meta.name]);
        if (panel) buildPanel(stream, panel);
        if (meta.name === 'optics' || meta.name === 'ppg') {
          // Visibility is showStages' job; this only builds the chart.
          var pp = document.getElementById('panel-pulse');
          if (pp) buildPulse(stream, pp);
        }
      }
    });

    state.built = true;
    resizeCharts();
  }

  function rebuild() {
    if (!state.built) return;
    var saved = streams.map(function (s) { return s.trace; });
    build({ streams: streams.map(function (s) { return s.meta; }) });
    streams.forEach(function (s, i) {
      if (saved[i] && saved[i].nchan === s.trace.nchan) s.trace = traceResize(saved[i], s.trace.target);
    });
  }

  function resizeCharts() {
    streams.forEach(function (s) {
      s.charts.forEach(function (u, i) {
        var node = s.readouts[i] ? s.readouts[i].node : u.root.parentNode;
        var w = chartWidth(node || u.root.parentNode);
        if (w !== u.width) u.setSize({ width: w, height: u.height });
      });
    });
    streams.forEach(function (s) {
      if (!s.pulseChart) return;
      var w = chartWidth(s.pulseChart.root.parentNode);
      if (w !== s.pulseChart.width) s.pulseChart.setSize({ width: w, height: s.pulseChart.height });
    });
  }

  // ----------------------------------------------------------- draw loop

  var lastDraw = 0;
  var lastStats = 0;

  function draw(now) {
    requestAnimationFrame(draw);
    if (now - lastDraw < DRAW_INTERVAL) return;
    lastDraw = now;

    var doStats = now - lastStats >= STATS_INTERVAL;
    if (doStats) lastStats = now;

    streams.forEach(function (s) {
      if (!s.dirty && !doStats) return;
      s.dirty = false;

      var tr = s.trace;
      if (tr.len === 0) return;

      var xs = tr.x.subarray(0, tr.len);
      var xmax = xs[tr.len - 1];
      var xmin = xmax - state.window;

      if (doStats) updateStats(s, xs, xmin);

      if (s.meta.name === 'eeg') {
        s.charts.forEach(function (u, i) {
          u.setData([xs, tr.y[i].subarray(0, tr.len)], false);
          u.setScale('y', { min: s.center[i] - state.scale, max: s.center[i] + state.scale });
          u.setScale('x', { min: xmin, max: xmax });
        });
      } else if (s.charts.length) {
        var data = [xs];
        for (var c = 0; c < tr.nchan; c++) data.push(tr.y[c].subarray(0, tr.len));
        // true lets uPlot re-range y: PPG counts and accelerometer g have no
        // useful fixed range, unlike EEG's explicit amplitude setting.
        s.charts[0].setData(data, true);
        s.charts[0].setScale('x', { min: xmin, max: xmax });
      }

      if (s.pulseChart) {
        var ci = 0;
        if (pulse.channel) {
          var at = s.meta.channels.indexOf(pulse.channel);
          if (at >= 0) ci = at;
        }
        var ys = tr.y[ci].subarray(0, tr.len);
        // Beats are absolute Unix seconds; xs is relative to state.epoch (see
        // tracePush's caller), so shift before comparing.
        var beats = state.epoch === null ? [] : pulse.beats.map(function (t) { return t - state.epoch; });
        s.pulseChart.setData([xs, ys, beatSeries(xs, tr.len, ys, beats)], true);
        s.pulseChart.setScale('x', { min: xmin, max: xmax });
      }
    });

    if (now - rateStamp >= 1000) {
      if (el.linkRate) {
        el.linkRate.textContent = framesThisSecond > 0 ? framesThisSecond + '/s' : '';
      }
      framesThisSecond = 0;
      rateStamp = now;
    }
  }

  // updateStats writes the per-channel standard deviation of the visible
  // window next to the channel label — the same signal-quality cue as
  // muse-lsl's viewer.
  function updateStats(s, xs, xmin) {
    var tr = s.trace;
    // The visible window is a suffix of the buffer: find where it starts.
    var start = 0;
    for (var i = tr.len - 1; i >= 0; i--) {
      if (xs[i] < xmin) { start = i + 1; break; }
    }
    var n = tr.len - start;
    if (n < 2) return;

    for (var c = 0; c < tr.nchan; c++) {
      var view = tr.y[c].subarray(start, tr.len);
      var sd = stdDev(view, n);
      s.center[c] = meanOf(view, n);

      var out = s.readouts[c];
      if (!out) continue;
      out.sigma.textContent = 'σ ' + formatValue(sd, displayUnit(s.meta.unit));

      // On a wide stream, a channel the device is not filling reads as exactly
      // zero rather than as noise — Athena optics decodes all 16 canonical
      // channels but a given preset only carries 4 or 8 of them. Hide those,
      // so the panel shows the live channels instead of a flat line at zero
      // dragging the shared y-scale down. Restricted to wide streams because
      // on a 3-axis IMU an exact zero is more likely to be real.
      if (tr.nchan > WIDE_STREAM && s.silent) {
        var silent = sd === 0 && s.center[c] === 0;
        if (silent !== s.silent[c]) {
          s.silent[c] = silent;
          if (s.charts[0]) s.charts[0].setSeries(c + 1, { show: !silent });
          if (out.chip) out.chip.dataset.silent = silent ? 'true' : 'false';
        }
      }

      if (out.quality) {
        var level = sd < 20 ? 'good' : (sd < 60 ? 'warning' : 'critical');
        var word = level === 'good' ? 'clean' : (level === 'warning' ? 'noisy' : 'poor');
        if (out.quality.dataset.level !== level) {
          out.quality.dataset.level = level;
          out.word.textContent = word;
        }
        out.fill.style.width = Math.min(100, (sd / 80) * 100).toFixed(0) + '%';
      }
    }
  }

  // The wire format spells the EEG unit "uV" because it travels as ASCII;
  // the page can afford the real character.
  function displayUnit(unit) { return unit === 'uV' ? 'µV' : unit; }

  function formatValue(v, unit) {
    var abs = Math.abs(v);
    var text;
    if (abs >= 100000) text = v.toExponential(1);
    else if (abs >= 100) text = v.toFixed(0);
    else if (abs >= 1) text = v.toFixed(1);
    else text = v.toFixed(3);
    return unit ? text + ' ' + unit : text;
  }

  // ------------------------------------------------------------- controls

  function send(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
  }

  function setScale(next) {
    state.scale = Math.min(MAX_SCALE, Math.max(MIN_SCALE, next));
    el.scaleValue.textContent = '± ' + formatValue(state.scale, 'µV');
    streams.forEach(function (s) { s.dirty = true; });
  }

  function setWindow(next) {
    next = Math.min(MAX_WINDOW, Math.max(MIN_WINDOW, Math.round(next)));
    if (next === state.window) return;
    state.window = next;
    el.windowValue.textContent = state.window + ' s';
    streams.forEach(function (s) {
      s.trace = traceResize(s.trace, Math.ceil(state.window * s.meta.rate));
      s.dirty = true;
    });
    send({ window: state.window });
  }

  function setFilter(on) {
    state.filter = on;
    el.filterToggle.setAttribute('aria-pressed', String(on));
    el.filterLabel.textContent = on ? 'Filter on' : 'Filter off';
    send({ filter: on });
  }

  document.getElementById('scaleUp').addEventListener('click', function () { setScale(state.scale * 2); });
  document.getElementById('scaleDown').addEventListener('click', function () { setScale(state.scale / 2); });
  document.getElementById('windowUp').addEventListener('click', function () { setWindow(state.window + 1); });
  document.getElementById('windowDown').addEventListener('click', function () { setWindow(state.window - 1); });
  el.filterToggle.addEventListener('click', function () { setFilter(!state.filter); });

  document.addEventListener('keydown', function (e) {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    var t = e.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;

    switch (e.key) {
      case '/': setScale(state.scale * 2); break;
      case '*': setScale(state.scale / 2); break;
      case '+': case '=': setWindow(state.window + 1); break;
      case '-': case '_': setWindow(state.window - 1); break;
      case 'd': case 'D': setFilter(!state.filter); break;
      default: return;
    }
    e.preventDefault();
  });

  window.addEventListener('resize', resizeCharts);

  // A collapsed <details> has zero width, so its chart has to be re-measured
  // the moment it opens.
  Array.prototype.forEach.call(document.querySelectorAll('details.collapsible'), function (d) {
    d.addEventListener('toggle', resizeCharts);
  });

  var darkQuery = window.matchMedia('(prefers-color-scheme: dark)');
  if (darkQuery.addEventListener) darkQuery.addEventListener('change', rebuild);

  // ------------------------------------------------------------ transport

  function setLink(stateName, text) {
    el.link.dataset.state = stateName;
    el.linkText.textContent = text;
  }

  function connect() {
    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    setLink('connecting', 'Connecting');

    ws = new WebSocket(proto + '//' + location.host + '/ws');
    ws.binaryType = 'arraybuffer';

    ws.onopen = function () {
      backoff = 500;
      setLink('live', 'Live');
      // Restore this viewer's settings on the fresh connection.
      send({ filter: state.filter, window: state.window });
    };

    ws.onmessage = function (e) {
      if (typeof e.data === 'string') {
        var msg;
        try { msg = JSON.parse(e.data); } catch (err) { return; }
        if (msg && msg.type === 'hello' && !state.built) build(msg);
        else if (msg && msg.type === 'pulse') applyPulse(msg);
        return;
      }
      var frame = decodeFrame(e.data);
      if (!frame) return;
      framesThisSecond++;

      var s = streamsById[frame.stream];
      if (!s) return;
      if (state.epoch === null) state.epoch = frame.t0;
      tracePush(s.trace, frame.t0 - state.epoch, frame.dt, frame.chans);
      s.dirty = true;
    };

    ws.onerror = function () { /* onclose does the retrying */ };

    ws.onclose = function () {
      setLink('down', 'Reconnecting');
      var wait = backoff + Math.random() * 250;
      backoff = Math.min(backoff * 2, 10000);
      setTimeout(connect, wait);
    };
  }

  setScale(state.scale);
  el.windowValue.textContent = state.window + ' s';
  setFilter(false);
  requestAnimationFrame(draw);
  connect();
})();
