// go-speedtest UI controller (main thread).
//
// Loads /config.json, drives the measurement Web Worker, renders live progress
// and final results, POSTs the completed result to the telemetry endpoint, and
// renders a shareable PNG card client-side. No frameworks, no build step.
'use strict';

(function () {
  var GAUGE_CIRC = 2 * Math.PI * 88; // matches r=88 in the SVG

  var el = {
    serverName: document.getElementById('server-name'),
    gaugePhase: document.getElementById('gauge-phase'),
    gaugeValue: document.getElementById('gauge-value'),
    gaugeUnit: document.getElementById('gauge-unit'),
    gaugeFill: document.getElementById('gauge-fill'),
    mDownload: document.getElementById('m-download'),
    mUpload: document.getElementById('m-upload'),
    mPing: document.getElementById('m-ping'),
    mJitter: document.getElementById('m-jitter'),
    metrics: document.querySelectorAll('.metric'),
    startBtn: document.getElementById('start-btn'),
    saveBtn: document.getElementById('save-btn'),
    clientIp: document.getElementById('client-ip'),
    status: document.getElementById('status'),
    canvas: document.getElementById('share-canvas'),
  };

  var PHASE_COLOR = {
    ping: getVar('--accent'),
    download: getVar('--down'),
    upload: getVar('--up'),
  };
  var PHASE_LABEL = { ping: 'Ping', download: 'Download', upload: 'Upload' };

  var cfg = null;
  var worker = null;
  var running = false;
  var lastResult = null;

  init();

  function init() {
    fetch('config.json', { cache: 'no-store' })
      .then(function (r) { return r.json(); })
      .then(function (c) {
        cfg = c;
        el.serverName.textContent = c.server_name || '';
        loadIP();
      })
      .catch(function () {
        setStatus('Failed to load configuration.');
        el.startBtn.disabled = true;
      });

    el.startBtn.addEventListener('click', function () {
      if (running) abort();
      else start();
    });
    el.saveBtn.addEventListener('click', saveImage);
  }

  function loadIP() {
    fetch(cfg.endpoints.ip, { cache: 'no-store' })
      .then(function (r) { return r.json(); })
      .then(function (d) { el.clientIp.textContent = d && d.ip ? d.ip : 'unknown'; })
      .catch(function () { el.clientIp.textContent = 'unknown'; });
  }

  // ---- test lifecycle ----

  function start() {
    running = true;
    lastResult = null;
    resetMetrics();
    el.saveBtn.hidden = true;
    setStatus('');
    setButtonAbort(true);

    worker = new Worker('worker.js');
    worker.onmessage = onWorkerMessage;
    worker.onerror = function (e) {
      setStatus('Worker error: ' + (e.message || 'unknown'));
      finishRun();
    };
    worker.postMessage({ type: 'start', config: cfg });
  }

  function abort() {
    if (worker) worker.postMessage({ type: 'abort' });
    setStatus('Aborted.');
    setGauge('ping', 0, 0, 'Ready', 'Mbps'); // clear
    el.gaugePhase.textContent = 'Ready';
    finishRun();
  }

  function finishRun() {
    running = false;
    setButtonAbort(false);
    clearActive();
    if (worker) {
      worker.terminate();
      worker = null;
    }
  }

  function onWorkerMessage(e) {
    var m = e.data || {};
    switch (m.type) {
      case 'phase':
        onPhase(m);
        break;
      case 'progress':
        setActive(m.phase);
        setGauge(m.phase, m.mbps, m.pct, PHASE_LABEL[m.phase], 'Mbps');
        setMetric(m.phase, fmt(m.mbps));
        break;
      case 'ping':
        setActive('ping');
        setGauge('ping', m.ping, m.pct || 0, 'Ping', 'ms');
        setMetric('ping', fmt(m.ping));
        setMetric('jitter', fmt(m.jitter));
        break;
      case 'result':
        onResult(m.result);
        break;
      case 'aborted':
        finishRun();
        break;
      case 'error':
        setStatus(m.message || 'Test error.');
        finishRun();
        break;
    }
  }

  function onPhase(m) {
    if (m.status === 'start') {
      setActive(m.phase);
      setGauge(m.phase, 0, 0, PHASE_LABEL[m.phase], m.phase === 'ping' ? 'ms' : 'Mbps');
    } else if (m.status === 'done') {
      if (m.phase === 'ping') {
        setMetric('ping', fmt(m.ping));
        setMetric('jitter', fmt(m.jitter));
      } else {
        setMetric(m.phase, fmt(m.mbps));
      }
    }
  }

  function onResult(result) {
    lastResult = result;
    setMetric('download', fmt(result.download_mbps));
    setMetric('upload', fmt(result.upload_mbps));
    setMetric('ping', fmt(result.ping_ms));
    setMetric('jitter', fmt(result.jitter_ms));
    setGauge('download', result.download_mbps, 1, 'Done', 'Mbps');
    el.saveBtn.hidden = false;
    setStatus('Test complete.');
    finishRun();
    submitResult(result);
  }

  // POST the result; telemetry may be disabled (404/501) -> ignore silently.
  function submitResult(result) {
    fetch(cfg.endpoints.results, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(result),
    }).catch(function () {});
  }

  // ---- rendering ----

  function setGauge(phase, value, pct, label, unit) {
    el.gaugeValue.textContent = fmt(value);
    el.gaugeUnit.textContent = unit;
    el.gaugePhase.textContent = label;
    var color = PHASE_COLOR[phase] || getVar('--accent');
    el.gaugeFill.style.stroke = color;
    var p = Math.max(0, Math.min(1, pct || 0));
    el.gaugeFill.style.strokeDashoffset = String(GAUGE_CIRC * (1 - p));
  }

  function setMetric(name, text) {
    var node = el['m' + cap(name)];
    if (node) node.textContent = text;
  }

  function resetMetrics() {
    setMetric('download', '--');
    setMetric('upload', '--');
    setMetric('ping', '--');
    setMetric('jitter', '--');
    setGauge('ping', 0, 0, 'Ready', 'Mbps');
  }

  function setActive(phase) {
    el.metrics.forEach(function (node) {
      node.classList.toggle('active', node.getAttribute('data-metric') === phase);
    });
  }
  function clearActive() {
    el.metrics.forEach(function (node) { node.classList.remove('active'); });
  }

  function setButtonAbort(on) {
    el.startBtn.textContent = on ? 'Abort' : 'Start';
    el.startBtn.classList.toggle('abort', on);
  }

  function setStatus(text) { el.status.textContent = text; }

  // ---- share image ----

  function saveImage() {
    if (!lastResult) return;
    drawCard(el.canvas, lastResult);
    el.canvas.toBlob(function (blob) {
      if (!blob) return;
      var url = URL.createObjectURL(blob);
      var a = document.createElement('a');
      a.href = url;
      a.download = 'go-speedtest-' + Date.now() + '.png';
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
    }, 'image/png');
  }

  // Fixed dark palette so the shared card looks consistent regardless of theme.
  function drawCard(canvas, r) {
    var W = canvas.width, H = canvas.height;
    var ctx = canvas.getContext('2d');
    ctx.fillStyle = '#10151d';
    ctx.fillRect(0, 0, W, H);
    ctx.fillStyle = '#1a222d';
    roundRect(ctx, 32, 32, W - 64, H - 64, 22);
    ctx.fill();

    ctx.fillStyle = '#4f97f0';
    ctx.beginPath();
    ctx.arc(78, 92, 9, 0, Math.PI * 2);
    ctx.fill();

    ctx.fillStyle = '#e8edf3';
    ctx.font = '600 34px system-ui, sans-serif';
    ctx.textBaseline = 'middle';
    ctx.fillText('go-speedtest', 100, 92);

    ctx.fillStyle = '#8b98a7';
    ctx.font = '400 20px system-ui, sans-serif';
    ctx.textAlign = 'right';
    ctx.fillText((cfg && cfg.server_name) || '', W - 64, 92);
    ctx.textAlign = 'left';

    var cells = [
      { label: 'DOWNLOAD', value: fmt(r.download_mbps), unit: 'Mbps', color: '#4f97f0' },
      { label: 'UPLOAD', value: fmt(r.upload_mbps), unit: 'Mbps', color: '#2bd4a4' },
      { label: 'PING', value: fmt(r.ping_ms), unit: 'ms', color: '#e8edf3' },
      { label: 'JITTER', value: fmt(r.jitter_ms), unit: 'ms', color: '#e8edf3' },
    ];
    var gx = 72, gy = 176, cw = (W - 144) / 2, ch = 148;
    cells.forEach(function (c, i) {
      var x = gx + (i % 2) * cw;
      var y = gy + Math.floor(i / 2) * ch;
      ctx.fillStyle = '#8b98a7';
      ctx.font = '600 16px system-ui, sans-serif';
      ctx.fillText(c.label, x, y + 14);
      ctx.fillStyle = c.color;
      ctx.font = '700 56px system-ui, sans-serif';
      ctx.fillText(c.value, x, y + 62);
      var valueWidth = ctx.measureText(c.value).width;
      ctx.fillStyle = '#8b98a7';
      ctx.font = '400 22px system-ui, sans-serif';
      ctx.fillText(c.unit, x + valueWidth + 8, y + 62);
    });

    ctx.fillStyle = '#5a6675';
    ctx.font = '400 18px system-ui, sans-serif';
    ctx.fillText(new Date().toLocaleString(), 72, H - 60);
  }

  function roundRect(ctx, x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
  }

  // ---- helpers ----

  function fmt(v) {
    if (v == null || isNaN(v)) return '--';
    if (v >= 100) return String(Math.round(v));
    if (v >= 10) return v.toFixed(1);
    return v.toFixed(2);
  }
  function cap(s) { return s.charAt(0).toUpperCase() + s.slice(1); }
  function getVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || '#2f7de1';
  }
})();
