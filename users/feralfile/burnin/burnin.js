// burnin.js: the logic of the factory burn-in page (index.html), kept free of
// the DOM so scripts/test-ff1-burnin.sh can run it in node.
//
// The page is served by ff1-burnin.sh's loopback HTTP server, next to
// /status.json, which the script rewrites every few seconds: the unit-check
// results, and the system sample (temperatures, kernel errors, adapters,
// Chromium restarts). The page adds what only it can measure: frame rate per
// segment and the video's dropped frames. Everything here is a pure function
// of those two inputs.
'use strict';

// One pass, in order. slides is the display-check page's assembly set, whose
// slide lengths sum to SLIDES_SECONDS (the test pins the sum). Each pass ends
// with the status card.
const SLIDES_SECONDS = 102;
// ready is a selector that matches in the segment's document only once its
// test content runs: the display-check page has put a slide on its stage,
// evolved has compiled its shaders and written the formula, p5 has made its
// canvas. An error page or a work that failed to start matches nothing.
// panel is the Panel Check of FF-Acceptance-Packing-Spec §1: black 15 s,
// gray 15 s, nothing else on screen (clean: no status dot), long enough for a
// 60 s phone capture plus the power pull while still recording. It opens a
// pass, so the status card before it can announce it, and only once the unit
// has been on PANEL_MIN_UPTIME (the spec's 10 minutes, counted from boot).
const PANEL_MIN_UPTIME = 600;
const SEGMENTS = [
  { id: 'panel', seconds: 120, src: '/display-check/index.html?set=panel', ready: '#stage > *', judged: false, clean: true },
  { id: 'slides', seconds: SLIDES_SECONDS, src: '/display-check/index.html', ready: '#stage > *', judged: true },
  { id: 'evolved', seconds: 60, src: '/burnin/load/evolved/index.html', ready: '#overlay:not(:empty)', judged: false },
  { id: 'uneasy', seconds: 60, src: '/burnin/load/uneasy-dream/index.html', ready: 'canvas', judged: false },
  { id: 'video', seconds: 60, video: '/burnin/load/clip-4k60-hevc10.mp4', judged: true },
];
// A segment that is not ready, or a clip that has not started playing, within
// this long is a failure; the pass moves on to the next segment.
const START_SECONDS = 15;
const CLIP_FPS = 60;          // the clip's frame rate
const CARD_SECONDS = 8;       // the status card between passes
const RESULT_SECONDS = 60;    // the result card after the unit checks
const WARMUP_SECONDS = 3;     // frames of a segment's first seconds are not judged

// Red, once any of these is crossed, until the power is pulled. The WebGL
// works are GPU-bound at 4K and may run below the panel rate on a healthy
// unit, so their frame rate is shown but never judged (judged: false above).
const LIMITS = {
  minFps: 50,            // slides and video, until the panel rate is measured
  minShare: 0.83,        // slides and video: share of the panel rate (50 of 60, 25 of 30)
  maxDropShare: 0.01,    // video: dropped / decoded frames, per pass
  cpuC: 95,              // k10temp Tctl
  gpuC: 100,             // amdgpu edge
  nvmeC: 80,             // nvme composite
  staleSeconds: 90,      // status.json older than this: the sampler stopped
};

function emptyStats() {
  return { passes: 0, fps: {}, dropped: 0, decoded: 0, bad: [], refresh: null };
}

// withRefresh(stats, fps) -> stats judged against the panel's own rate,
// measured on a still card (a station at 4K@30 or 50 Hz would otherwise read
// red on every healthy unit).
function withRefresh(stats, fps) {
  return fps > 0 ? { ...stats, refresh: fps } : stats;
}

function minFps(stats) {
  return stats.refresh ? stats.refresh * LIMITS.minShare : LIMITS.minFps;
}

// recordSegment(stats, id, frames, seconds, judged) -> stats with the
// segment's average frame rate folded in (the worst pass is kept).
function recordSegment(stats, id, frames, seconds, judged) {
  const fps = seconds > 0 ? frames / seconds : 0;
  const prev = stats.fps[id];
  const out = { ...stats, fps: { ...stats.fps, [id]: prev === undefined ? fps : Math.min(prev, fps) } };
  if (judged && fps < minFps(stats)) out.bad = addBad(out.bad, `${id} ${fps.toFixed(1)} fps`);
  return out;
}

// dropAllowance(stats) -> the share of the clip's frames that may drop. A
// panel slower than the clip cannot show every frame, and Chromium counts the
// ones it never showed as dropped (half of them at 30 Hz, one in six at 50).
function dropAllowance(stats) {
  const r = stats.refresh;
  return LIMITS.maxDropShare + (r && r < CLIP_FPS ? 1 - r / CLIP_FPS : 0);
}

// recordVideo(stats, dropped, decoded) -> stats with one pass of the clip.
function recordVideo(stats, dropped, decoded) {
  const out = { ...stats, dropped: stats.dropped + dropped, decoded: stats.decoded + decoded };
  // Nothing decoded in a whole segment: the clip did not play at all.
  if (decoded === 0) out.bad = addBad(out.bad, 'video decoded 0 frames');
  else if (dropped / decoded > dropAllowance(stats)) {
    out.bad = addBad(out.bad, `video dropped ${dropped}/${decoded}`);
  }
  return out;
}

// passSegments(uptimeSeconds) -> the segments of a pass starting now: the
// Panel Check only once the unit has been on long enough (null: not known
// yet, so not yet).
function passSegments(uptimeSeconds) {
  return SEGMENTS.filter(seg => seg.id !== 'panel' || (uptimeSeconds ?? 0) >= PANEL_MIN_UPTIME);
}

// fail(stats, reason) -> stats with reason latched for the rest of the session.
function fail(stats, reason) {
  return { ...stats, bad: addBad(stats.bad, reason) };
}

// Staleness is measured on the page's monotonic clock: how long since a read
// last brought a new updated_at. Not wall time minus updated_at: timesyncd
// steps the clock on this boot, and a step of minutes or hours (an RTC kept
// in local time) would read as a stopped sampler on a healthy unit.
// trackFreshness(fresh, updatedAt, monoSeconds) -> fresh, restarted when
// updatedAt changed; staleFor(fresh, monoSeconds) -> seconds without change.
function trackFreshness(fresh, updatedAt, monoSeconds) {
  return updatedAt !== fresh.seen ? { seen: updatedAt, at: monoSeconds } : fresh;
}
function staleFor(fresh, monoSeconds) {
  return monoSeconds - fresh.at;
}

// latchStatus(stats, staleSeconds) -> stats with a stale status latched: a
// sampler that stopped and came back must stay on the record.
function latchStatus(stats, staleSeconds) {
  return staleSeconds > LIMITS.staleSeconds ? fail(stats, 'status sampler stopped') : stats;
}

// The latched reasons are a set: the same fault again (with other numbers)
// does not grow the list, and the first occurrence is kept.
function addBad(bad, reason) {
  const key = r => r.replace(/[0-9.\/]+/g, '#');
  return bad.some(r => key(r) === key(reason)) ? bad : bad.concat(reason);
}

// verdict(status, stats, staleSeconds) -> { ok, reasons }: red if anything has
// failed this session. status may be null (not read yet); staleSeconds is
// staleFor() of the reads so far (0 when unknown).
function verdict(status, stats, staleSeconds = 0) {
  const reasons = [...stats.bad];
  if (!status) return { ok: reasons.length === 0, reasons };
  for (const c of status.checks || []) {
    if (c.status === 'FAIL') reasons.push(`${c.name} FAIL`);
  }
  const s = status.sys || {};
  if (s.kernel_errors > 0) reasons.push(`kernel errors ${s.kernel_errors}`);
  if (s.restarts > 0) reasons.push(`Chromium restarts ${s.restarts}`);
  if (s.wifi === 'lost') reasons.push('Wi-Fi adapter gone');
  if (s.bt === 'lost') reasons.push('Bluetooth adapter gone');
  if (s.cpu_max >= LIMITS.cpuC) reasons.push(`CPU ${s.cpu_max} °C`);
  if (s.gpu_max >= LIMITS.gpuC) reasons.push(`GPU ${s.gpu_max} °C`);
  if (s.nvme_max >= LIMITS.nvmeC) reasons.push(`NVMe ${s.nvme_max} °C`);
  if (staleSeconds > LIMITS.staleSeconds) reasons.push('status sampler stopped');
  const unique = [...new Set(reasons)];
  return { ok: unique.length === 0, reasons: unique };
}

// hms(seconds) -> "h:mm:ss" (hours not capped: a run can last days).
function hms(seconds) {
  const t = Math.max(0, Math.floor(seconds));
  const h = Math.floor(t / 3600), m = Math.floor(t / 60) % 60, s = t % 60;
  return `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

// qrPayload(status, stats, nowSeconds) -> the compact JSON a phone scans in
// place of a photograph. Short keys keep it within a QR code a phone reads
// from across a bench (QR_MAX_BYTES; the test checks a worst case).
const QR_MAX_BYTES = 400;
function qrPayload(status, stats, runningSeconds, staleSeconds = 0) {
  const st = status || {};
  const s = st.sys || {};
  const v = verdict(status, stats, staleSeconds);
  const fps = id => (stats.fps[id] === undefined ? null : Math.round(stats.fps[id] * 10) / 10);
  const p = {
    ff1: 'burnin/1',
    b: st.board || null,
    v: st.version || null,
    t: runningSeconds === null || runningSeconds === undefined ? null : Math.max(0, Math.floor(runningSeconds)),
    n: stats.passes,
    ok: v.ok ? 1 : 0,
    c: Object.fromEntries((st.checks || []).map(c => [c.name.slice(0, 4), c.status])),
    f: SEGMENTS.map(seg => fps(seg.id)),
    d: [stats.dropped, stats.decoded],
    T: [s.cpu_max ?? null, s.gpu_max ?? null, s.nvme_max ?? null],
    e: s.kernel_errors ?? null,
    r: s.restarts ?? null,
  };
  // The reasons go in one by one while the payload stays within budget; the
  // status card on screen always lists them all.
  let out = JSON.stringify(p);
  const why = [];
  for (const r of v.reasons) {
    const next = JSON.stringify({ ...p, why: why.concat(r.slice(0, 32)) });
    if (byteLength(next) > QR_MAX_BYTES) break;
    why.push(r.slice(0, 32)); out = next;
  }
  return out;
}

function byteLength(s) {
  return typeof TextEncoder !== 'undefined' ? new TextEncoder().encode(s).length : s.length;
}

// The corner the status dot sits in for pass n: it moves every pass, so no
// pixel holds the same content for days.
const CORNERS = ['top-left', 'top-right', 'bottom-right', 'bottom-left'];
function cornerFor(pass) {
  return CORNERS[pass % CORNERS.length];
}

// resultCardLeft(status, uptimeNow) -> seconds the result card has left, on
// the monotonic uptime; 0 or less once over (a Chromium restart after that goes
// straight to the loop) or while unknown.
function resultCardLeft(status, uptimeNow) {
  if (!status || status.checks_done_uptime === null || status.checks_done_uptime === undefined || uptimeNow === null) return 0;
  return status.checks_done_uptime + RESULT_SECONDS - uptimeNow;
}

// The page's estimate of the unit's uptime: the sampled /proc/uptime plus
// the page's monotonic time since. advanceUptime(clock, sampled, mono) moves
// it on a read: only when the sampled value changed (the same file is read
// more than once between samples), and never backwards, so the running time
// on screen never repeats or reverses a second. uptimeAt(clock, mono) reads
// it (null while no sample has arrived).
function advanceUptime(clock, sampled, mono) {
  if (typeof sampled !== 'number') return clock;
  if (clock && clock.sampled === sampled) return clock;
  const est = uptimeAt(clock, mono);
  return { sampled, up: est === null ? sampled : Math.max(sampled, est), mono };
}
function uptimeAt(clock, mono) {
  return clock ? clock.up + (mono - clock.mono) : null;
}

// running(status, uptimeNow) -> seconds this session has run, on the monotonic
// uptime (null while unknown).
function running(status, uptimeNow) {
  if (!status || status.started_uptime === null || status.started_uptime === undefined || uptimeNow === null) return null;
  return Math.max(0, uptimeNow - status.started_uptime);
}

const api = {
  SLIDES_SECONDS, SEGMENTS, PANEL_MIN_UPTIME, passSegments, START_SECONDS, CARD_SECONDS, RESULT_SECONDS, WARMUP_SECONDS, LIMITS, QR_MAX_BYTES,
  emptyStats, fail, trackFreshness, staleFor, latchStatus, withRefresh, minFps, dropAllowance, recordSegment, recordVideo, verdict, hms, qrPayload, cornerFor, resultCardLeft, running, advanceUptime, uptimeAt,
};
if (typeof module !== 'undefined') module.exports = api;
else window.Burnin = api;
