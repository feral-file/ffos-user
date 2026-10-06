import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('templates/result.html', import.meta.url), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const flush = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };

function page({ fetchAvailable = true, abortAvailable = true } = {}) {
  const timers = new Map();
  const requests = [];
  const navigations = [];
  const listeners = {};
  let id = 0;
  const document = {
    visibilityState: 'visible',
    addEventListener: (name, fn) => { listeners[name] = fn; },
  };
  const fetch = (url, opts) => new Promise((resolve, reject) => {
    requests.push({ url, opts, resolve, reject });
    opts.signal.addEventListener('abort', () => reject(new Error('aborted')));
  });
  vm.runInNewContext(script, {
    window: {
      fetch: fetchAvailable ? fetch : undefined,
      AbortController: abortAvailable ? AbortController : undefined,
      location: { replace: url => navigations.push(url) },
    },
    document, fetch, AbortController,
    setTimeout: (fn, ms) => { timers.set(++id, { fn, ms }); return id; },
    clearTimeout: id => timers.delete(id),
  });
  return {
    requests, navigations, document,
    visible: () => listeners.visibilitychange?.(),
    tick(ms) {
      const timer = [...timers].find(([, t]) => t.ms === ms);
      assert.ok(timer, `expected ${ms}ms timer`);
      timers.delete(timer[0]);
      timer[1].fn();
    },
    timers,
  };
}

const answer = (request, state) => request.resolve({ ok: true, json: async () => ({ state }) });

test('polls immediately, survives AP outage, and GET-navigates to failure banner', async () => {
  const p = page();
  assert.equal(p.requests.length, 1);
  assert.equal(p.requests[0].url, '/status');
  assert.equal(p.requests[0].opts.cache, 'no-store');
  assert.equal(p.requests[0].opts.headers['X-Setup-Watcher'], '1');
  p.visible();
  assert.equal(p.requests.length, 1, 'no overlapping requests');
  p.tick(1500);
  await flush();
  assert.ok(p.requests[0].opts.signal.aborted);
  p.tick(3000);
  answer(p.requests[1], 'joining');
  await flush();
  p.tick(3000);
  answer(p.requests[2], 'failed');
  await flush();
  assert.deepEqual(p.navigations, ['/']);
  assert.equal(p.timers.size, 0);
  p.visible();
  assert.equal(p.requests.length, 3, 'stops after navigation');
});

test('HTTP errors, malformed JSON, and non-failure states never invent a failure', async () => {
  const p = page();
  for (const response of [
    { ok: false },
    { ok: true, json: async () => { throw new Error('invalid JSON'); } },
    ...['idle', 'joining', 'succeeded', 'unknown'].map(state => ({ ok: true, json: async () => ({ state }) })),
  ]) {
    p.requests.at(-1).resolve(response);
    await flush();
    assert.deepEqual(p.navigations, []);
    p.tick(3000);
  }
});

test('timeout covers stalled response body and visibility resumes polling', async () => {
  const p = page();
  const req = p.requests[0];
  req.resolve({ ok: true, json: () => new Promise((resolve, reject) => {
    req.opts.signal.addEventListener('abort', () => reject(new Error('aborted body')));
  }) });
  await flush();
  p.tick(1500);
  await flush();
  p.visible();
  assert.equal(p.requests.length, 2);
  answer(p.requests[1], 'failed');
  await flush();
  assert.deepEqual(p.navigations, ['/']);
});

test('unsupported browsers retain plain HTML reconnect instructions', () => {
  for (const options of [{ fetchAvailable: false }, { abortAvailable: false }]) {
    const p = page(options);
    assert.equal(p.requests.length, 0);
    assert.equal(p.timers.size, 0);
  }
  assert.match(html, /scan the QR code on its screen again/);
});
