#!/usr/bin/env bash
# Pins the factory burn-in (docs/plans/factory-burnin.md): ff1-burnin.sh's
# helpers, the burn-in page's logic (burnin.js, in node) and its assets. The
# boot side (entry, target, updater drop-ins) is pinned in ffos
# scripts/verify.sh.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
burnin="$repo_root/users/feralfile/scripts/ff1-burnin.sh"
page_dir="$repo_root/users/feralfile/burnin"
display_page="$repo_root/users/feralfile/display-check/index.html"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "test-ff1-burnin: $*" >&2
  exit 1
}

grep -Fq "chmod +x /home/feralfile/scripts/ff1-burnin.sh" "$repo_root/users/feralfile/.file_permissions.sh" || \
  fail "device install does not make ff1-burnin.sh executable"

# The burn-in must leave nothing a normal boot reads: no owner/setup state, no
# NetworkManager connections, no systemctl enable/disable.
if grep -vE '^[[:space:]]*#' "$burnin" | grep -Eq '\.state|NetworkManager/system-connections|nmcli (c|con|connection) (add|modify|delete)|systemctl (enable|disable|mask)'; then
  fail "ff1-burnin.sh writes state a normal boot reads"
fi

# --- assets -------------------------------------------------------------------
# Everything the page loads exists, and the burn-in's share of the normal
# image stays within its budget (plan §4.4).
for f in index.html burnin.js qrcode.js load/evolved/index.html load/evolved/main.js \
  load/evolved/vertex.glslv load/evolved/x-zebradoorkijk.glslf load/uneasy-dream/index.html \
  load/uneasy-dream/p5.min.js load/uneasy-dream/sketch.js load/clip-4k60-hevc10.mp4; do
  [ -s "$page_dir/$f" ] || fail "burnin/$f is missing"
done
for ref in /display-check/index.html /burnin/load/evolved/index.html /burnin/load/uneasy-dream/index.html /burnin/load/clip-4k60-hevc10.mp4; do
  grep -Fq "'$ref'" "$page_dir/burnin.js" || fail "burnin.js no longer plays $ref"
done
budget=$((12 * 1024 * 1024))
size=$(du -sb "$page_dir" | cut -f1)
[ "$size" -le "$budget" ] || fail "burnin/ is $size bytes, over the 12 MB budget"
[ -s "$display_page" ] || fail "the display-check page the slides segment shows is missing"

# A segment's frame counts as its own only with the query too: the Panel Check
# is the display-check page with ?set=panel.
grep -Fq 'doc.location.pathname + doc.location.search === seg.src' "$page_dir/index.html" || \
  fail "index.html's frameReady must compare the path and the query"

# The clip must keep playing for the whole segment (a freeze after it started
# is a failure), and a card too full for the panel shrinks rather than cuts.
grep -Fq "B.fail(stats, 'video stalled')" "$page_dir/index.html" || fail "index.html no longer watches the clip's progress"
grep -Fq 'fitCard();' "$page_dir/index.html" || fail "index.html no longer fits the card to the panel"

# --- ff1-burnin.sh helpers ----------------------------------------------------
# shellcheck source=/dev/null
source "$burnin"

# wifi_line: per band, APs and the best median; none seen is a FAIL.
cat >"$tmp_dir/ok.json" <<'JSON'
{"scans": 10, "summary": [
  {"band": "2.4GHz", "median_dbm": -61.5}, {"band": "2.4GHz", "median_dbm": -48},
  {"band": "5GHz", "median_dbm": -70}]}
JSON
got=$(wifi_line "$tmp_dir/ok.json")
[ "$got" = "== Wi-Fi      PASS   2.4GHz: 2 APs, best -48 dBm; 5GHz: 1 APs, best -70 dBm" ] || fail "wifi_line ok: '$got'"
echo '{"scans": 10, "summary": []}' >"$tmp_dir/none.json"
got=$(wifi_line "$tmp_dir/none.json")
[ "$got" = "== Wi-Fi      FAIL   no access point seen in 10 scans" ] || fail "wifi_line none: '$got'"
got=$(wifi_line "$tmp_dir/missing.json")
[[ "$got" == "== Wi-Fi      FAIL"* ]] || fail "wifi_line missing file: '$got'"

# result_lines keeps the English result lines only, and says so when none came.
got=$(result_lines $'noise\n== Brightness PASS   a\n-- 亮度 通过\n== Speakers   PLAYED b')
[ "$got" = $'== Brightness PASS   a\n== Speakers   PLAYED b' ] || fail "result_lines: '$got'"
[ "$(result_lines 'nothing')" = "== Checks     FAIL   did not run" ] || fail "result_lines empty"

# checks_json: the lines unit-checks.sh prints ("== %-10s %-6s %s").
got=$(checks_json "$(printf '== %-10s %-6s %s\n' Brightness PASS "50 -> 80 -> 50" Speakers PLAYED "" Bluetooth FAIL "no adapter")" | jq -c .)
[ "$got" = '[{"name":"Brightness","status":"PASS","detail":"50 -> 80 -> 50"},{"name":"Speakers","status":"PLAYED","detail":""},{"name":"Bluetooth","status":"FAIL","detail":"no adapter"}]' ] || \
  fail "checks_json: $got"

# temps: k10temp, amdgpu and nvme temp1, in whole °C; a missing one is null.
for s in "0 k10temp 71250" "1 amdgpu 64000" "2 acpitz 40000"; do
  read -r i name milli <<<"$s"
  mkdir -p "$tmp_dir/hwmon/hwmon$i"; echo "$name" >"$tmp_dir/hwmon/hwmon$i/name"; echo "$milli" >"$tmp_dir/hwmon/hwmon$i/temp1_input"
done
got=$(HWMON="$tmp_dir/hwmon" temps)
[ "$got" = "71 64 null" ] || fail "temps: '$got'"

# max treats null as no reading.
[ "$(max null 5)" = 5 ] && [ "$(max 7 null)" = 7 ] && [ "$(max 3 9)" = 9 ] && [ "$(max null null)" = null ] || fail "max"

# count_kernel_errors: faults count, ordinary driver chatter does not.
got=$(count_kernel_errors <<'LOG'
amdgpu 0000:03:00.0: amdgpu: SMU is initialized successfully!
amdgpu 0000:03:00.0: [drm:amdgpu_job_timedout [amdgpu]] *ERROR* ring gfx_0.0.0 timeout, signaled seq=1, emitted seq=2
amdgpu 0000:03:00.0: amdgpu: GPU reset begin!
nvme nvme0: I/O 12 QID 3 timeout, aborting
nvme0: Admin Cmd(0x6), I/O Error (sct 0x0 / sc 0x2)
rtw_8821ce 0000:02:00.0: firmware: direct-loading firmware rtw88/rtw8821c_fw.bin
mce: [Hardware Error]: Machine check events logged
LOG
)
[ "${got%%$'\t'*}" = 5 ] || fail "count_kernel_errors counted '${got%%$'\t'*}', want 5 (ring timeout, GPU reset, two NVMe, the machine check line)"
[ "$(count_kernel_errors </dev/null)" = $'0\t' ] || fail "count_kernel_errors on nothing"

# previous_session: the newest earlier last.json; this session's is ignored.
mkdir -p "$tmp_dir/log/20260101-000000" "$tmp_dir/log/20260102-000000" "$tmp_dir/log/20260103-000000"
echo '{"started_at":1000,"updated_at":4600,"sys":{"kernel_errors":0,"restarts":1}}' >"$tmp_dir/log/20260101-000000/last.json"
echo '{"started_at":5000,"updated_at":27200,"sys":{"kernel_errors":2,"restarts":0}}' >"$tmp_dir/log/20260102-000000/last.json"
echo '{"started_at":9,"updated_at":9,"sys":{}}' >"$tmp_dir/log/20260103-000000/last.json"
got=$(previous_session "$tmp_dir/log" 20260103-000000)
[ "$got" = '{"ran":22200,"last":"1970-01-01T07:33:20Z","kernel_errors":2,"restarts":0}' ] || fail "previous_session: $got"
[ "$(previous_session "$tmp_dir/empty" x)" = null ] || fail "previous_session with no sessions"
# An empty last.json (a power pull, a full disk) reads as no previous session,
# not as an empty string that would break every status.json after it.
mkdir -p "$tmp_dir/log2/20260101-000000"; : >"$tmp_dir/log2/20260101-000000/last.json"
[ "$(previous_session "$tmp_dir/log2" x)" = null ] || fail "previous_session with an empty last.json"
echo '{"started_at":1' >"$tmp_dir/log2/20260101-000000/last.json"
[ "$(previous_session "$tmp_dir/log2" x)" = null ] || fail "previous_session with a torn last.json"

# adapter_state: unseen until it appears, lost for good once it has gone.
mkdir -p "$tmp_dir/net"
st=unseen
st=$(adapter_state "$st" "$tmp_dir/net/wl*"); [ "$st" = unseen ] || fail "adapter_state before the adapter appears: $st"
mkdir "$tmp_dir/net/wlan0"
st=$(adapter_state "$st" "$tmp_dir/net/wl*"); [ "$st" = present ] || fail "adapter_state once present: $st"
rmdir "$tmp_dir/net/wlan0"
st=$(adapter_state "$st" "$tmp_dir/net/wl*"); [ "$st" = lost ] || fail "adapter_state once gone: $st"
mkdir "$tmp_dir/net/wlan0"
st=$(adapter_state "$st" "$tmp_dir/net/wl*"); [ "$st" = lost ] || fail "adapter_state must stay lost when it comes back: $st"

# prune_sessions keeps the newest N folders.
prune_sessions "$tmp_dir/log" 2
[ "$(find "$tmp_dir/log" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort | tr '\n' ' ')" = "20260102-000000 20260103-000000 " ] || \
  fail "prune_sessions did not keep the newest two"

# status_json: the shape burnin.js reads.
RUN_DIR="$tmp_dir/run"; mkdir -p "$RUN_DIR"
# shellcheck disable=SC2034 # read by status_json
BOARD=FF1-TEST VERSION=3.1.0 STARTED_AT=1000 PREVIOUS=null
echo '[{"name":"Brightness","status":"PASS","detail":""}]' >"$RUN_DIR/checks.json"
echo 1100 >"$RUN_DIR/checks_done_at"; echo /var/log/ff1-burnin/x >"$RUN_DIR/where"; echo 2 >"$RUN_DIR/restarts"
echo "612.34 1000.00" >"$tmp_dir/uptime"
# shellcheck disable=SC2034 # read by status_json
UPTIME="$tmp_dir/uptime"
got=$(status_json 70 60 null 75 61 null $'3\tGPU reset begin!' present lost | jq -c '[.board, .started_at, .checks_done_at, .checks[0].status, .sys.cpu_max, .sys.nvme_max, .sys.kernel_errors, .sys.last_error, .sys.wifi, .sys.bt, .sys.restarts, .sys.uptime, (.updated_at > 0)]')
[ "$got" = '["FF1-TEST",1000,1100,"PASS",75,null,3,"GPU reset begin!","present","lost",2,612,true]' ] || fail "status_json: $got"
rm -f "$RUN_DIR/checks.json" "$RUN_DIR/checks_done_at"
got=$(status_json null null null null null null $'0\t' unseen unseen | jq -c '[.checks, .checks_done_at, .sys.last_error]')
[ "$got" = '[[],null,null]' ] || fail "status_json before the checks: $got"

# results_stick: removable, unmounted, FAT/exFAT partitions, never on a disk
# that carries an iso9660 partition (an installer stick left plugged in).
mkdir -p "$tmp_dir/bin"
cat >"$tmp_dir/bin/lsblk" <<'STUB'
#!/usr/bin/env bash
# NAME TYPE RM PKNAME FSTYPE MOUNTPOINT. sdd is an installer stick: iso9660
# plus the ISO's FAT ESP, and comes first so a missing exclusion picks it.
printf '%s\n' "nvme0n1 disk 0   " "nvme0n1p1 part 0 nvme0n1 vfat /boot" "sdd disk 1   " "sdd1 part 1 sdd iso9660 " "sdd2 part 1 sdd vfat " \
  "sda disk 1   " "sda1 part 1 sda ext4 " "sdb1 part 1 sdb vfat /media/x" "sdc1 part 1 sdc exfat "
STUB
chmod +x "$tmp_dir/bin/lsblk"
got=$(PATH="$tmp_dir/bin:$PATH" results_stick) || fail "results_stick found none"
[ "$got" = /dev/sdc1 ] || fail "results_stick: '$got'"

# --- the page logic (burnin.js) ----------------------------------------------
command -v node >/dev/null || fail "node is needed for the page tests"
DISPLAY_PAGE="$display_page" PAGE_DIR="$page_dir" node - <<'NODE' || fail "burnin.js tests failed"
const assert = require('assert');
const fs = require('fs');
const B = require(process.env.PAGE_DIR + '/burnin.js');
const qrcode = require(process.env.PAGE_DIR + '/qrcode.js');

// The slides segment lasts exactly one pass of the display-check assembly set.
const page = fs.readFileSync(process.env.DISPLAY_PAGE, 'utf8');
const ids = page.match(/^\s*assembly:\s*\[(.*)\]/m)[1].replace(/['\s]/g, '').split(',');
const sum = ids.reduce((a, id) => a + Number(page.match(new RegExp(`\\{ id: '${id}',\\s*s: *(\\d+)`))[1]), 0);
assert.strictEqual(B.SLIDES_SECONDS, sum, `SLIDES_SECONDS ${B.SLIDES_SECONDS} != assembly pass ${sum}`);

const now = 10000;
const good = { board: 'FF1-ABCDEFGH', version: '3.1.0', started_at: now - 3600, updated_at: now - 4, checks_done_at: now - 3000,
  checks: [{ name: 'Brightness', status: 'PASS' }, { name: 'Speakers', status: 'PLAYED' }, { name: 'Wi-Fi', status: 'PASS' }],
  sys: { cpu_max: 80, gpu_max: 70, nvme_max: 50, kernel_errors: 0, restarts: 0, wifi: 'present', bt: 'present' } };

// Green when nothing failed; each fault turns it red with its reason.
let s = B.emptyStats();
assert.deepStrictEqual(B.verdict(good, s), { ok: true, reasons: [] });
assert.deepStrictEqual(B.verdict(null, s), { ok: true, reasons: [] });
const red = (patch, want) => {
  const st = JSON.parse(JSON.stringify(good)); patch(st);
  const v = B.verdict(st, s);
  assert.strictEqual(v.ok, false, want); assert.ok(v.reasons.some(r => r.startsWith(want)), `${want}: ${v.reasons}`);
};
red(st => { st.checks[2].status = 'FAIL'; }, 'Wi-Fi FAIL');
red(st => { st.sys.kernel_errors = 1; }, 'kernel errors');
red(st => { st.sys.restarts = 1; }, 'Chromium restarts');
red(st => { st.sys.bt = 'lost'; }, 'Bluetooth adapter gone');
// Not seen yet (the interface can take a minute at boot) is not a failure.
const unseen = JSON.parse(JSON.stringify(good)); unseen.sys.wifi = 'unseen';
assert.strictEqual(B.verdict(unseen, s).ok, true);
red(st => { st.sys.cpu_max = 95; }, 'CPU');
red(st => { st.sys.nvme_max = 80; }, 'NVMe');

// SKIP and PLAYED are not failures.
const skip = JSON.parse(JSON.stringify(good)); skip.checks[0].status = 'SKIP';
assert.strictEqual(B.verdict(skip, s).ok, true);

// Frame rate: judged segments latch red below the limit, the WebGL works never.
s = B.recordSegment(s, 'evolved', 20 * 57, 57, false);
assert.strictEqual(B.verdict(good, s).ok, true, 'a slow WebGL work is not a failure');
s = B.recordSegment(s, 'slides', 60 * 99, 99, true);
assert.strictEqual(B.verdict(good, s).ok, true);
s = B.recordSegment(s, 'slides', 30 * 99, 99, true);
assert.strictEqual(s.fps.slides, 30, 'the worst pass is kept');
assert.strictEqual(B.verdict(good, s).ok, false);
s = B.recordSegment(s, 'slides', 60 * 99, 99, true);
assert.strictEqual(B.verdict(good, s).ok, false, 'red stays red');
assert.strictEqual(s.bad.length, 1, 'the same fault does not grow the list');

// Judged against the panel's measured rate: 28 fps on a 30 Hz station is fine.
let r30 = B.withRefresh(B.emptyStats(), 30);
assert.strictEqual(B.minFps(r30), 30 * B.LIMITS.minShare);
r30 = B.recordSegment(r30, 'slides', 28 * 99, 99, true);
assert.strictEqual(B.verdict(good, r30).ok, true, '28 fps on a 30 Hz panel');
r30 = B.recordSegment(r30, 'slides', 20 * 99, 99, true);
assert.strictEqual(B.verdict(good, r30).ok, false, '20 fps on a 30 Hz panel');
assert.strictEqual(B.withRefresh(B.emptyStats(), 0).refresh, null, 'a failed measurement keeps the default');
// The 60 fps clip on a 30 Hz panel: half its frames are never shown and count
// as dropped; that is not a fault. Beyond that allowance it is.
let v30 = B.recordVideo(B.withRefresh(B.emptyStats(), 30), 1800, 3600);
assert.strictEqual(B.verdict(good, v30).ok, true, '60 fps clip on a 30 Hz panel');
v30 = B.recordVideo(B.withRefresh(B.emptyStats(), 30), 2400, 3600);
assert.strictEqual(B.verdict(good, v30).ok, false, 'too many drops even for 30 Hz');
assert.strictEqual(B.dropAllowance(B.withRefresh(B.emptyStats(), 75)), B.LIMITS.maxDropShare, 'a faster panel gets no extra allowance');

// Video: more than 1 % dropped, or nothing decoded, is red.
let v = B.recordVideo(B.emptyStats(), 30, 3600);
assert.strictEqual(B.verdict(good, v).ok, true);
v = B.recordVideo(v, 50, 3600);
assert.strictEqual(B.verdict(good, v).ok, false);
assert.deepStrictEqual([v.dropped, v.decoded], [80, 7200]);
assert.strictEqual(B.verdict(good, B.recordVideo(B.emptyStats(), 0, 0)).ok, false);

// A stale status latches: a fresh one afterwards does not clear it.
// Staleness is the page's monotonic time since updated_at last changed.
let fr = B.trackFreshness({ seen: undefined, at: 0 }, 1000, 0);
fr = B.trackFreshness(fr, 1000, 80);
assert.strictEqual(B.staleFor(fr, 80), 80, 'the same updated_at does not refresh');
fr = B.trackFreshness(fr, 1005, 85);
assert.strictEqual(B.staleFor(fr, 86), 1, 'a new updated_at refreshes');
// A wall clock stepped forward by hours, with samples still arriving: green.
fr = B.trackFreshness(fr, 1005 + 8 * 3600, 90);
assert.strictEqual(B.verdict(good, B.latchStatus(B.emptyStats(), B.staleFor(fr, 91)), B.staleFor(fr, 91)).ok, true, 'a clock step is not a stopped sampler');
assert.strictEqual(B.verdict(good, B.emptyStats(), 91).ok, false, 'no new sample for 91 s is');
let ls = B.latchStatus(B.emptyStats(), 100);
assert.strictEqual(B.verdict(good, ls, 0).ok, false, 'stale latched');
ls = B.latchStatus(ls, 0);
assert.strictEqual(B.verdict(good, ls, 0).ok, false, 'still red after the sampler came back');
assert.strictEqual(B.verdict(good, ls, 100).reasons.length, 1, 'latched and live stale count once');

// Segment failures latch; different faults of one segment are both kept.
let sf = B.fail(B.emptyStats(), 'slides did not load');
sf = B.recordSegment(sf, 'slides', 30 * 99, 99, true);
sf = B.recordSegment(sf, 'slides', 31 * 99, 99, true);
assert.deepStrictEqual(sf.bad, ['slides did not load', 'slides 30.0 fps']);
assert.ok(B.SEGMENTS.every(seg => seg.video || seg.ready), 'every frame segment has a ready selector');

// The Panel Check (acceptance spec §1): black/gray only, a clean screen,
// opening a pass, and only after 10 minutes of power-on.
const panel = B.SEGMENTS[0];
assert.deepStrictEqual([panel.id, panel.src, panel.clean, panel.judged], ['panel', '/display-check/index.html?set=panel', true, false]);
assert.ok(panel.seconds >= 60 + 30, 'room for a 60 s capture, the reaction time and the power pull');
assert.ok(B.passSegments(null).every(seg => seg.id !== 'panel'), 'no uptime yet: no Panel Check');
assert.ok(B.passSegments(599).every(seg => seg.id !== 'panel'), 'before 10 minutes: no Panel Check');
assert.strictEqual(B.passSegments(600)[0].id, 'panel');
assert.strictEqual(B.passSegments(600).length, B.SEGMENTS.length);
assert.ok(/panel:\s*\['black','gray'\]/.test(page) && /panel: 15/.test(page), 'the display-check panel set is black/gray 15 s');

// Running time and corners.
assert.strictEqual(B.hms(0), '0:00:00');
assert.strictEqual(B.hms(3 * 86400 + 3661), '73:01:01');
assert.deepStrictEqual([0, 1, 2, 3, 4].map(B.cornerFor), ['top-left', 'top-right', 'bottom-right', 'bottom-left', 'top-left']);
assert.strictEqual(B.resultCardUntil(null), 0);
assert.strictEqual(B.resultCardUntil(good), good.checks_done_at + B.RESULT_SECONDS);

// QR payload: parseable, within budget in the worst case, and encodable.
const worst = JSON.parse(JSON.stringify(good));
worst.board = 'FF1-WWWWWWWW'; worst.version = '10.20.300-rc.12';
worst.checks = ['Brightness', 'Speakers', 'Bluetooth', 'Wi-Fi'].map(n => ({ name: n, status: 'FAIL', detail: 'x'.repeat(200) }));
Object.assign(worst.sys, { cpu_max: 105, gpu_max: 110, nvme_max: 90, kernel_errors: 123456, restarts: 9999, wifi: 'lost', bt: 'lost' });
worst.updated_at = 0;
let ws = B.emptyStats();
for (const seg of B.SEGMENTS) ws = B.recordSegment(ws, seg.id, 1, 99, true);
ws = B.recordVideo(ws, 999999, 999999);
ws.passes = 99999;
for (const [st, stats] of [[good, B.emptyStats()], [worst, ws], [null, B.emptyStats()]]) {
  const p = B.qrPayload(st, stats, now, st === worst ? 1000 : 0);
  assert.ok(Buffer.byteLength(p) <= B.QR_MAX_BYTES, `payload ${Buffer.byteLength(p)} bytes`);
  const j = JSON.parse(p);
  assert.strictEqual(j.ff1, 'burnin/1');
  const qr = qrcode(0, 'L'); qr.addData(p, 'Byte'); qr.make();
  assert.ok(qr.getModuleCount() <= 73, `QR version too large: ${qr.getModuleCount()} modules`);
}
const pg = JSON.parse(B.qrPayload(good, B.emptyStats(), now));
assert.deepStrictEqual([pg.b, pg.t, pg.ok, pg.c.Brig, pg.c['Wi-F']], ['FF1-ABCDEFGH', 3600, 1, 'PASS', 'PASS']);
assert.ok(Array.isArray(JSON.parse(B.qrPayload(worst, ws, now)).why), 'the reasons go in while they fit');
NODE

echo "test-ff1-burnin: ok"
