// go-speedtest measurement worker.
//
// Runs the entire transfer engine off the UI thread. The main thread only
// sends {type:'start', config} / {type:'abort'} and renders the progress,
// ping, phase, result, error and aborted messages posted back here.
//
// Transport is XHR ONLY for download/upload (fetch cannot measure upload wire
// delivery and miscounts under Content-Encoding). Ping prefers a WebSocket
// echo and falls back to timed XHR GETs. All timing uses performance.now().
'use strict';

var cfg = null; // UIConfig delivered by the main thread
var ep = null; // cfg.endpoints
var uploadBlob = null; // generated once, reused for every upload POST
var aborted = false;
var currentAbort = null; // cancels the in-flight phase and resolves its promise

// Ping accumulators (reset per ping test).
var minPing = Infinity;
var jitter = 0;
var prevRtt = null;

self.onmessage = function (e) {
  var msg = e.data || {};
  if (msg.type === 'start') {
    cfg = msg.config;
    ep = cfg.endpoints;
    runSequence();
  } else if (msg.type === 'abort') {
    aborted = true;
    if (currentAbort) currentAbort();
    post({ type: 'aborted' });
  }
};

function post(m) {
  self.postMessage(m);
}

// ---- ping / jitter ---------------------------------------------------------

function resetPing() {
  minPing = Infinity;
  jitter = 0;
  prevRtt = null;
}

// Asymmetric EWMA jitter + running-minimum ping (DESIGN.md methodology).
function addSample(rtt) {
  if (rtt < minPing) minPing = rtt;
  if (prevRtt !== null) {
    var inst = Math.abs(rtt - prevRtt);
    if (inst > jitter) jitter = 0.3 * jitter + 0.7 * inst;
    else jitter = 0.8 * jitter + 0.2 * inst;
  }
  prevRtt = rtt;
}

function postPing(progress) {
  post({
    type: 'ping',
    phase: 'ping',
    ping: minPing === Infinity ? 0 : minPing,
    jitter: jitter,
    pct: progress,
  });
}

function pingTest(done) {
  resetPing();
  var finished = false;
  function finish() {
    if (finished) return;
    finished = true;
    done({ ping: minPing === Infinity ? 0 : minPing, jitter: jitter });
  }
  currentAbort = finish;

  if (cfg.websocket_ping) {
    pingWS(finish);
  } else {
    pingXHR(finish);
  }
}

function pingWS(finish) {
  var proto = self.location.protocol === 'https:' ? 'wss:' : 'ws:';
  var url = proto + '//' + self.location.host + ep.ws;
  var ws;
  try {
    ws = new WebSocket(url);
  } catch (err) {
    pingXHR(finish);
    return;
  }
  var count = 0;
  var t0 = 0;
  var usedFallback = false;

  var failTimer = setTimeout(function () {
    if (ws.readyState !== 1 && count === 0) {
      usedFallback = true;
      try { ws.close(); } catch (e) {}
      pingXHR(finish);
    }
  }, 1500);

  function sendOne() {
    t0 = performance.now();
    try { ws.send('p'); } catch (e) {}
  }

  ws.onopen = function () {
    clearTimeout(failTimer);
    sendOne();
  };
  ws.onmessage = function () {
    if (aborted) { try { ws.close(); } catch (e) {} return; }
    addSample(performance.now() - t0);
    count++;
    postPing(count / cfg.ping_samples);
    if (count < cfg.ping_samples) {
      sendOne();
    } else {
      try { ws.close(); } catch (e) {}
      finish();
    }
  };
  ws.onerror = function () {
    clearTimeout(failTimer);
    if (!usedFallback && count === 0) {
      usedFallback = true;
      pingXHR(finish);
    }
  };
  ws.onclose = function () {
    // Closed after collecting at least one sample without completing the run:
    // report what we have rather than hanging.
    if (!usedFallback && count > 0 && count < cfg.ping_samples) finish();
  };
}

function pingXHR(finish) {
  var count = 0;
  function next() {
    if (aborted || count >= cfg.ping_samples) {
      finish();
      return;
    }
    var xhr = new XMLHttpRequest();
    var t0 = performance.now();
    xhr.onload = function () {
      addSample(performance.now() - t0);
      count++;
      postPing(count / cfg.ping_samples);
      next();
    };
    xhr.onerror = function () {
      count++;
      next();
    };
    xhr.open('GET', bust(ep.ping), true);
    xhr.send();
  }
  next();
}

// ---- transfer engine (download + upload share this) ------------------------

function bust(path) {
  return path + (path.indexOf('?') === -1 ? '?' : '&') + 'cb=' + Date.now() + '_' + Math.random();
}

function makeUploadBlob(sizeBytes) {
  var buf = new Uint8Array(sizeBytes);
  var MAX = 65536; // crypto.getRandomValues fills at most 65536 bytes per call
  for (var off = 0; off < sizeBytes; off += MAX) {
    var len = Math.min(MAX, sizeBytes - off);
    crypto.getRandomValues(buf.subarray(off, off + len));
  }
  return new Blob([buf]);
}

// runTransfer drives `streams` parallel XHRs (spawned by spawnFn) for
// test_duration_ms, resetting the byte/time counters after graceMs to discard
// TCP slow-start, then reports throughput.
function runTransfer(phase, streams, graceMs, spawnFn, done) {
  var state = {
    running: true,
    loaded: 0,
    xhrs: [],
    graced: false,
    graceLoaded: 0,
    graceTime: 0,
  };
  var t0 = performance.now();
  var finished = false;

  function stop() {
    state.running = false;
    clearInterval(iv);
    clearTimeout(graceTimer);
    clearTimeout(endTimer);
    for (var i = 0; i < state.xhrs.length; i++) {
      try { state.xhrs[i].abort(); } catch (e) {}
    }
  }

  function compute(now) {
    var start = state.graced ? state.graceTime : t0;
    var base = state.graced ? state.graceLoaded : 0;
    var bytes = state.loaded - base;
    var durSec = (now - start) / 1000;
    var mbps = durSec > 0 ? (bytes * 8 / durSec / 1e6) * cfg.overhead_factor : 0;
    return { mbps: mbps, bytes: bytes, durationMs: Math.round(durSec * 1000) };
  }

  function finish() {
    if (finished) return;
    finished = true;
    stop();
    done(compute(performance.now()));
  }
  currentAbort = finish;

  var graceTimer = setTimeout(function () {
    state.graced = true;
    state.graceLoaded = state.loaded;
    state.graceTime = performance.now();
  }, graceMs);

  var iv = setInterval(function () {
    if (aborted) return;
    var now = performance.now();
    var r = compute(now);
    post({
      type: 'progress',
      phase: phase,
      mbps: r.mbps,
      pct: Math.min(1, (now - t0) / cfg.test_duration_ms),
    });
  }, 150);

  var endTimer = setTimeout(finish, cfg.test_duration_ms);

  for (var i = 0; i < streams; i++) spawnFn(state, i);
}

function spawnDownload(state, i) {
  var xhr = new XMLHttpRequest();
  state.xhrs[i] = xhr;
  var last = 0;
  xhr.onprogress = function (e) {
    var d = e.loaded - last;
    last = e.loaded;
    if (d > 0) state.loaded += d;
  };
  xhr.onload = function () {
    if (state.running) spawnDownload(state, i); // restart stream until phase ends
  };
  xhr.onerror = function () {
    if (state.running) setTimeout(function () { if (state.running) spawnDownload(state, i); }, 100);
  };
  xhr.responseType = 'arraybuffer';
  xhr.open('GET', bust(ep.download) + '&chunks=' + cfg.download_chunks, true);
  xhr.send();
}

function spawnUpload(state, i) {
  var xhr = new XMLHttpRequest();
  state.xhrs[i] = xhr;
  var last = 0;
  xhr.upload.onprogress = function (e) {
    var d = e.loaded - last;
    last = e.loaded;
    if (d > 0) state.loaded += d; // measure wire delivery, not response
  };
  xhr.onload = function () {
    if (state.running) spawnUpload(state, i);
  };
  xhr.onerror = function () {
    if (state.running) setTimeout(function () { if (state.running) spawnUpload(state, i); }, 100);
  };
  xhr.open('POST', bust(ep.upload), true);
  xhr.setRequestHeader('Content-Type', 'application/octet-stream');
  xhr.send(uploadBlob);
}

// ---- sequence --------------------------------------------------------------

function runSequence() {
  aborted = false;

  post({ type: 'phase', phase: 'ping', status: 'start' });
  pingTest(function (ping) {
    if (aborted) return;
    post({ type: 'phase', phase: 'ping', status: 'done', ping: ping.ping, jitter: ping.jitter });

    post({ type: 'phase', phase: 'download', status: 'start' });
    runTransfer('download', cfg.download_streams, cfg.grace_download_ms, spawnDownload, function (dl) {
      if (aborted) return;
      post({ type: 'phase', phase: 'download', status: 'done', mbps: dl.mbps });

      if (!uploadBlob) uploadBlob = makeUploadBlob(cfg.upload_blob_bytes);

      post({ type: 'phase', phase: 'upload', status: 'start' });
      runTransfer('upload', cfg.upload_streams, cfg.grace_upload_ms, spawnUpload, function (ul) {
        if (aborted) return;
        post({ type: 'phase', phase: 'upload', status: 'done', mbps: ul.mbps });

        currentAbort = null;
        post({
          type: 'result',
          result: {
            download_mbps: round2(dl.mbps),
            upload_mbps: round2(ul.mbps),
            ping_ms: round2(ping.ping),
            jitter_ms: round2(ping.jitter),
            download_bytes: dl.bytes,
            upload_bytes: ul.bytes,
            download_duration_ms: dl.durationMs,
            upload_duration_ms: ul.durationMs,
            streams_download: cfg.download_streams,
            streams_upload: cfg.upload_streams,
            overhead_factor: cfg.overhead_factor,
            source: 'web',
            server_name: cfg.server_name,
          },
        });
      });
    });
  });
}

function round2(v) {
  return Math.round(v * 100) / 100;
}
