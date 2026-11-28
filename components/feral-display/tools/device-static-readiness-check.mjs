// Run tests/delayed-static-server.py on the device first. Replaces its playlist.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import {connectNative} from './native-client.mjs';
const [socket, output] = process.argv.slice(2);
assert(socket && output);
const c = await connectNative(socket, 'static-readiness-check');
const evidence = {startedAt:new Date().toISOString(), physicalOutputVerified:false, samples:[]};
try {
  await c.call('connectivity.set', {online:true});
  const receipt = await c.call('playback.replace', {playlist:{id:'delayed-static',items:[
    {source:'http://127.0.0.1:18991/static.html',duration:0}]}});
  const start = performance.now();
  for (let i=0;i<100;i++) {
    await new Promise(r=>setTimeout(r,100));
    const s = await c.call('state.get');
    evidence.samples.push({elapsedMs:performance.now()-start,stage:s.selected?.stage,frames:s.media?.exportedFrames});
    assert.notEqual(s.selected?.stage,'failed',s.selected?.error);
    if (s.presented?.id===receipt.occurrence) {
      evidence.result='passed'; evidence.presentationMs=performance.now()-start; break;
    }
  }
  assert.equal(evidence.result,'passed','Static page never presented after its nonvisual load dependency');
} finally {
  c.close(); await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');
}
console.log(JSON.stringify({result:evidence.result,presentationMs:evidence.presentationMs}));
