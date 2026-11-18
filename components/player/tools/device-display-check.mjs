// Isolated-player regression: host composition acknowledgments and matte pixels.
import assert from 'node:assert/strict';
import {mkdir,writeFile} from 'node:fs/promises';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {join} from 'node:path';
import {connectNative} from './native-client.mjs';
const [socket,host,out]=process.argv.slice(2);assert(out,'usage: device-display-check.mjs SOCKET SSH-HOST OUTPUT');
const run=promisify(execFile),delay=ms=>new Promise(r=>setTimeout(r,ms));
const flags=['-o','IPQoS=none','-i',process.env.HOME+'/.ssh/id_ed25519','-o','BatchMode=yes','-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts'];
const c=await connectNative(socket,'display-check');await mkdir(out,{recursive:true});
const evidence={physicalOutputVerified:false,checks:[]};
async function waitFor(predicate){for(let n=0;n<150;n++){const s=await c.call('state.get');assert.notEqual(s.renderer,'failed',JSON.stringify(s.selected));if(predicate(s))return s;await delay(100);}throw Error('Composition did not complete');}
async function cast(source,display={}){const r=await c.call('playback.replace',{playlist:{items:[{source:'http://127.0.0.1:18990/'+source,duration:0,display}]}});return waitFor(s=>s.presented?.id===r.occurrence&&s.composition?.occurrence===r.occurrence);}
async function capture(name){
 const remote='/home/feralfile/ff-native-evidence/display-'+name+'.png',local=join(out,name+'.png');
 await run('ssh',[...flags,host,'XDG_RUNTIME_DIR=/run/user/1000 WAYLAND_DISPLAY=wayland-0 grim '+remote]);
 await run('scp',['-O',...flags,host+':'+remote,local]);
 const r=await run('python3',['-c','from PIL import Image;import json,sys;im=Image.open(sys.argv[1]).convert("RGB");print(json.dumps([im.getpixel((50,50)),im.getpixel((150,150))]))',local]);return JSON.parse(r.stdout);
}
async function preview(s,display){
 const receipt=await c.call('presentation.set',{occurrence:s.presented.id,display});
 assert.equal(receipt.acceptedCompositionRevision,s.composition.revision);
 const updated=await waitFor(x=>x.composition?.occurrence===s.presented.id&&x.composition.revision>receipt.acceptedCompositionRevision);
 for(const [key,value] of Object.entries(display))assert.deepEqual(updated.composition.display[key],value);
 return updated;
}
try{
 await c.call('overlay.set',{owner:'setup',state:'ready'});
 await c.call('settings.set',{tombstone:'off',durationSeconds:null,muted:true,framing:'artwork',margin:0,background:'#000000'});
 let s=await cast('presentation.html');
 s=await preview(s,{margin:100,background:'#00FF00'});
 assert.deepEqual(await capture('software'),[[0,255,0],[17,34,51]]);
 evidence.checks.push('software preview commits after configuration; native matte and inner viewport pixels match');
 const retired=s.presented.id;s=await cast('presentation.html');
 assert.equal(s.composition.display.margin,0);
 await assert.rejects(c.call('presentation.set',{occurrence:retired,display:{margin:200}}),e=>e.code==='stale_occurrence');
 evidence.checks.push('same URL replacement resets preview and rejects delayed prior-occurrence write');
 s=await cast('media-h264-aac.mp4',{autoPlay:false,loop:true});
 s=await preview(s,{margin:'10%',background:'#008000',scaling:'fill'});
 s=await waitFor(x=>x.media?.pause===true&&x.media.surfaceWidth===1536&&x.media.surfaceHeight===1536);
 assert.equal(s.composition.display.scaling,'fill');assert.deepEqual((await capture('paused-video'))[0],[0,128,0]);
 evidence.checks.push('paused video applies resize/scaling/color and emits a new configured frame without resuming');
 s=await preview(s,{margin:'10%'});
 evidence.checks.push('no-op preview receives a later causal composition revision');
 for(const background of ['#fff','white','rgba(255, 0, 0, 0.5)']){
  s=await cast('media-h264-aac.mp4',{autoPlay:false,loop:true,margin:100,background});
  assert.equal(s.composition.display.background,background);
  const expected=background.startsWith('rgba')?[128,0,0]:[255,255,255];
  assert.deepEqual((await capture('authored-'+background.replace(/[^a-z0-9]/gi,'')))[0],expected);
 }
 evidence.checks.push('authored CSS backgrounds resolve consistently for paused media and the native matte');
 const composed=s.composition;
 await c.call('overlay.set',{owner:'setup',state:'connecting'});
 await c.call('presentation.set',{occurrence:s.presented.id,display:{margin:200,background:'#FF0000'}});
 await delay(500);
 assert.deepEqual((await c.call('state.get')).composition,composed);
 await c.call('presentation.set',{occurrence:s.presented.id,display:{margin:300,background:'#0000FF'}});
 await c.call('overlay.set',{owner:'setup',state:'ready'});
 s=await waitFor(x=>x.composition?.display.margin===300&&x.composition.display.background==='#0000FF');
 assert.deepEqual((await capture('uncovered-preview'))[0],[0,0,255]);
 evidence.checks.push('covered superseding configuration retains prior composition and completes after uncovering');
 evidence.result='passed';
}catch(e){evidence.result='failed';evidence.error=String(e);process.exitCode=1;}
finally{c.close();await writeFile(join(out,'result.json'),JSON.stringify(evidence,null,2)+'\n');console.log(JSON.stringify(evidence));}
