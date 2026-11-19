// Synthetic raster acceptance on an isolated native-host socket. Captures are
// Cage compositor output, not photographs or proof of physical HDMI output.
import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {randomUUID} from 'node:crypto';
import {connectNative} from './native-client.mjs';
const run=promisify(execFile);
const [socket,host,directory]=process.argv.slice(2);
assert(directory&&/^[a-zA-Z0-9.@:-]+$/.test(host),'usage: device-native-image-check.mjs ISOLATED-SOCKET SSH-HOST OUTPUT-DIR');
const flags=['-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts','-i',process.env.HOME+'/.ssh/id_ed25519'];
const fixtures=JSON.parse(await fs.readFile(new URL('../tests/native-image-fixtures.json',import.meta.url)));
await fs.mkdir(directory,{recursive:true});
const remote='/tmp/ff-native-images-'+randomUUID();await run('ssh',[...flags,host,'mkdir','-p',remote]);
const c=await connectNative(socket,'native-image-check');const evidence={startedAt:new Date().toISOString(),checks:[],capture:'Cage compositor; physical output not verified'};
const delay=ms=>new Promise(r=>setTimeout(r,ms));
try{
 await c.call('connectivity.set',{online:true});
 await c.call('settings.set',{tombstone:'off',durationSeconds:null,background:'#ffffff',framing:'artwork'});
 for(const fixture of fixtures){
  const receipt=await c.call('playback.replace',{playlist:{items:[{...fixture,id:fixture.name,title:fixture.name,duration:0,display:{scaling:'fit',margin:0,background:'#ffffff'}}]}});
  let state;for(let i=0;i<200;i++){state=await c.call('state.get');if(state.selected?.stage==='failed')throw Error(state.selected.error);if(state.presented?.id===receipt.occurrence&&state.media?.backend==='native-raster')break;await delay(100);}
  assert.equal(state.presented?.id,receipt.occurrence);assert.equal(state.media?.backend,'native-raster');
  await delay(1100);state=await c.call('state.get');
  const samples=fixture.name==='animated-gif'?6:1;
  if(samples>1){
   // Capture on-device in one session; SSH/SCP round trips can phase-lock to a
   // short GIF loop and falsely sample the same color every time.
   await run('ssh',[...flags,host,`for i in 0 1 2 3 4 5; do WAYLAND_DISPLAY=wayland-0 grim ${remote}/${fixture.name}-$i.png; sleep 0.13; done`]);
  }else await run('ssh',[...flags,host,`WAYLAND_DISPLAY=wayland-0 grim ${remote}/${fixture.name}-0.png`]);
  for(let i=0;i<samples;i++){
   const name=`${fixture.name}-${i}.png`;
   await run('scp',['-O',...flags,`${host}:${remote}/${name}`,path.join(directory,name)]);
  }
  evidence.checks.push({name:fixture.name,occurrence:receipt.occurrence,media:state.media});
 }
 evidence.result='native_decode_and_presentation_passed_pixels_pending';
}catch(error){evidence.result='failed';evidence.error=String(error);throw error;}
finally{c.close();evidence.finishedAt=new Date().toISOString();await fs.writeFile(path.join(directory,'result.json'),JSON.stringify(evidence,null,2)+'\n');}
