// Run with Node 22+ and the WebSocket URL of a disposable Chromium page.
// This exercises Blink's actual plain-text parser, including CR normalization.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { diagnosticSession } from '../tools/diagnostic-cdp.mjs';

const header = await readFile(new URL('../include/ff/plain_text_layout.hpp', import.meta.url), 'utf8');
const source = header.match(/R"JS\(([\s\S]+)\)JS"/)[1];
const page = await diagnosticSession(process.argv[2]);
const text = 'Photoshop launched\r\tOpen\r\r'.repeat(12000) + 'x'.repeat(80000) + '\rLast command';
const cases = [
  {mime: 'text/plain', body: text, chunked: true},
  {mime: 'text/plain', body: 'x'.repeat(16383) + '😀'.repeat(42000), chunked: true},
  {mime: 'text/plain', body: 'A short work', chunked: false},
  {mime: 'text/html', body: '<pre>An authored web page</pre>', chunked: false},
];

async function inspect(mime, body) {
  const url = 'data:' + mime + ';charset=utf-8;base64,' + Buffer.from(body).toString('base64');
  await page.call('Page.navigate', {url});
  const {frameTree} = await page.call('Page.getFrameTree');
  assert.equal(frameTree.frame.url, url);
  const {result, exceptionDetails} = await page.call('Runtime.evaluate', {
    expression: `(async () => {
      if (document.readyState !== 'complete')
        await new Promise(resolve => window.addEventListener('load', resolve, {once: true}));
      const pre = document.querySelector('pre');
      const range = document.createRange();
      range.selectNodeContents(pre);
      const selection = getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      return {text: pre.textContent, selected: selection.toString(),
              chunked: pre.children.length > 0, hidden: pre.hidden};
    })()`,
    awaitPromise: true,
    returnByValue: true,
  });
  assert.equal(exceptionDetails, undefined);
  return result.value;
}

try {
  await page.call('Page.enable');
  const baselines = [];
  for (const {mime, body} of cases) baselines.push(await inspect(mime, body));
  await page.call('Page.addScriptToEvaluateOnNewDocument', {source, worldName: 'ff-plain-text-layout'});
  for (const [index, {mime, body, chunked}] of cases.entries()) {
    assert.deepEqual(await inspect(mime, body), {...baselines[index], chunked, hidden: false});
  }
  console.log('Plain-text layout preserved decoded text and browser selection.');
} finally {
  page.close();
}
