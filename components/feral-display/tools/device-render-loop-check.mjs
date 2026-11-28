// Regression for busy GPU frames starving host commands/presentation. Run only
// against an isolated test player: this replaces its playlist and settings.
import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {createHash} from 'node:crypto';
import {connectNative} from './native-client.mjs';

const [socket, host, output] = process.argv.slice(2);
if (!output) throw Error('usage: device-render-loop-check.mjs SOCKET SSH-HOST OUTPUT-DIRECTORY');
const run = promisify(execFile);
const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
const flags = ['-o', 'IPQoS=none', '-i', process.env.HOME + '/.ssh/id_ed25519',
  '-o', 'BatchMode=yes', '-o', 'UserKnownHostsFile=/tmp/ff-player-native-known-hosts'];
const corpus = JSON.parse(await fs.readFile(new URL('../tests/living-code.json', import.meta.url), 'utf8'));
const work = corpus.playlist.items[7];
assert.equal(work.label.title, 'Transparent Grit');
const evidence = {startedAt: new Date().toISOString(), source: work.source,
  physicalOutputVerified: false, occurrences: [], roundTripMs: [], captures: []};
await fs.mkdir(output, {recursive: true});
const client = await connectNative(socket, 'render-loop-regression');
async function call(method, params) {
  const start = performance.now();
  const result = await client.call(method, params);
  evidence.roundTripMs.push(performance.now() - start);
  return result;
}
async function capture(index) {
  const remote = `/home/feralfile/ff-native-evidence/render-loop-${index}.png`;
  await run('ssh', [...flags, host, `XDG_RUNTIME_DIR=/run/user/1000 WAYLAND_DISPLAY=wayland-0 grim ${remote}`]);
  const file = path.join(output, `compositor-${index}.png`);
  await run('scp', ['-O', ...flags, `${host}:${remote}`, file]);
  evidence.captures.push({file: path.basename(file), sha256: createHash('sha256').update(await fs.readFile(file)).digest('hex')});
}
try {
  await call('settings.set', {durationSeconds: null, tombstone: 'on', muted: true});
  await call('connectivity.set', {online: true});
  for (let occurrence = 0; occurrence < 2; occurrence++) {
    const receipt = await call('playback.replace', {playlist: {id: 'gpu-loop-regression', title: 'Living Code',
      items: [{...work, id: 'grit', title: work.label.title, duration: 0, metadata: {artists: [{name: work.label.artist}]}}]}});
    evidence.occurrences.push(receipt.occurrence);
    const deadline = performance.now() + 30000;
    let presented = false;
    while (performance.now() < deadline) {
      const state = await call('state.get');
      assert.notEqual(state.selected?.stage, 'failed', JSON.stringify(state.selected));
      if (state.presented?.id === receipt.occurrence) {presented = true; break;}
      await wait(100);
    }
    assert(presented, 'No post-ready frame presented within 30 seconds');
    const end = performance.now() + 15000;
    while (performance.now() < end) {
      const state = await call('state.get');
      assert.equal(state.selected.id, receipt.occurrence, 'Unexpected renderer recovery');
      assert.equal(state.presented.id, receipt.occurrence);
      await wait(100);
    }
  }
  assert.notEqual(evidence.occurrences[0], evidence.occurrences[1]);
  await capture(0);
  await wait(2000);
  await capture(1);
  assert.notEqual(evidence.captures[0].sha256, evidence.captures[1].sha256,
    'Compositor output did not change during animated artwork');
  const latency = [...evidence.roundTripMs].sort((a,b) => a-b);
  evidence.latency = {count: latency.length, medianMs: latency[Math.floor(latency.length*.5)],
    p95Ms: latency[Math.floor(latency.length*.95)], maxMs: latency.at(-1)};
  assert(evidence.latency.maxMs < 2000, 'Host commands stalled under GPU rendering');
  evidence.result = 'passed_needs_capture_review';
} catch (error) {
  evidence.result = 'failed'; evidence.error = String(error); process.exitCode = 1;
} finally {
  client.close(); evidence.finishedAt = new Date().toISOString();
  await fs.writeFile(path.join(output, 'result.json'), JSON.stringify(evidence, null, 2) + '\n');
  console.log(JSON.stringify({result: evidence.result, latency: evidence.latency, error: evidence.error}));
}
