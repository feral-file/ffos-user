// One-shot migration/diagnostics tool; never a normal playback dependency.
// Node >=22, using built-in WebSocket. Reads through the live browser APIs.
import {writeFile} from 'node:fs/promises';
import {diagnosticSession} from './diagnostic-cdp.mjs';
const [endpoint, output, capture] = process.argv.slice(2);
if (!endpoint || !output || (capture && capture !== '--screenshot')) throw new Error('Usage: node legacy-export.mjs http://127.0.0.1:9222 output.json [--screenshot]');
const base = new URL(endpoint);
if (!['127.0.0.1', 'localhost', '[::1]'].includes(base.hostname)) throw new Error('Use a loopback endpoint or SSH tunnel');
const targets = await (await fetch(new URL('/json', base))).json();
const pages = targets.filter(x => x.type === 'page' && /^http:\/\/127\.0\.0\.1:8080\//.test(x.url));
if (pages.length !== 1) throw new Error('Expected exactly one legacy player target');
const address = new URL(pages[0].webSocketDebuggerUrl); address.host = base.host;
const session = await diagnosticSession(address);
const call = session.call;
try {
  const keys = ['castInfo','displaySettings','viewMode','criticalTemp','dp1_scheduled_tasks','boot_playlist','defaultItemDuration','recentlyPlayed','recentlyPlayedIncomplete','contentPolicy'];
  const expression = `(async () => {
    const keys = ${JSON.stringify(keys)};
    const readRuntime = async () => {
      if (typeof window.__ffosPlayerStatus !== 'function' || typeof window.handleCDPRequest !== 'function') throw new Error('Legacy player is not ready for migration');
      const probe = JSON.parse(window.__ffosPlayerStatus());
      if (probe.bootHydration === 'pending' || probe.bootHydration === 'failed') throw new Error('Legacy boot hydration is incomplete');
      const reply = JSON.parse(await window.handleCDPRequest({command:'checkStatus',request:{}}));
      const status = reply.message;
      if (!status || status.ok === false) throw new Error('Legacy status failed');
      return {route:location.pathname, bootHydration:probe.bootHydration, hasArtwork:probe.hasArtwork,
        cast:{playlist:status.playlist ?? null,index:status.index ?? 0,playlistUrl:status.playlistUrl ?? '',
          contentContext:status.contentContext ?? 'curated',loopMode:status.loopMode ?? 'playlist',
          shuffle:status.shuffle ?? false,isPaused:status.isPaused ?? false}};
    };
    const before = await readRuntime();
    const records = Object.fromEntries(keys.map(key => [key, null]));
    const databases = await indexedDB.databases();
    if (databases.some(db => db.name === 'FeralFileDisplayDB')) {
      await new Promise((resolve, reject) => {
        const open = indexedDB.open('FeralFileDisplayDB');
        open.onerror = () => reject(open.error);
        open.onupgradeneeded = () => {open.transaction.abort(); reject(new Error('Database disappeared'));};
        open.onsuccess = () => {
          const db = open.result; const tx = db.transaction('keyValueStore','readonly');
          tx.onabort = () => {db.close(); reject(tx.error ?? new Error('Read aborted'));};
          tx.onerror = () => {db.close(); reject(tx.error);};
          tx.oncomplete = () => {db.close(); resolve();};
          for (const key of keys) {const request = tx.objectStore('keyValueStore').get(key); request.onsuccess = () => {records[key] = request.result ?? null;};}
        };
      });
    }
    for (const key of keys) if (records[key] === null) records[key] = localStorage.getItem(key);
    const runtime = await readRuntime();
    if (JSON.stringify(before) !== JSON.stringify(runtime)) throw new Error('Playback changed during export; retry after commands settle');
    return {format:'ff-player-storage-export',version:2,origin:location.origin,exportedAt:new Date().toISOString(),records,runtime};
  })()`;
  const result = await call('Runtime.evaluate', {expression, awaitPromise:true, returnByValue:true});
  if (result.exceptionDetails || !result.result?.value) throw new Error('Legacy storage read failed: '+JSON.stringify(result.exceptionDetails));
  const document = JSON.stringify(result.result.value, null, 2) + '\n';
  if (Buffer.byteLength(document) > 4*1024*1024) throw new Error('Export exceeds migration limit');
  await writeFile(output, document, {flag:'wx', mode:0o600});
  console.log('Exported legacy state; no browser database files opened.');
  if (capture) {
    const screenshot = await call('Page.captureScreenshot', {format:'jpeg',quality:90});
    await writeFile(output+'.renderer.jpg', Buffer.from(screenshot.data,'base64'), {flag:'wx',mode:0o600});
    console.log('Captured renderer screenshot; this does not prove physical panel output.');
  }
} finally {session.close();}
