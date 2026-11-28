// Static artwork is deliberate: device UI invalidations must work without an
// artist animation causing incidental redraws. Captures are compositor output.
import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {connectNative} from './native-client.mjs';
const [socket,host,output]=process.argv.slice(2);
assert(output,'usage: device-presentation-check.mjs SOCKET SSH-HOST OUTPUT-DIRECTORY');
const run=promisify(execFile),delay=ms=>new Promise(r=>setTimeout(r,ms));
const flags=['-o','IPQoS=none','-i',process.env.HOME+'/.ssh/id_ed25519','-o','BatchMode=yes','-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts'];
await fs.mkdir(output,{recursive:true});
const evidence={physicalOutputVerified:false,checks:[]},c=await connectNative(socket,'composition-regression');
async function capture(name) {
 const remote='/home/feralfile/ff-native-evidence/presentation-'+name+'.png',local=path.join(output,name+'.png');
 await run('ssh',[...flags,host,`XDG_RUNTIME_DIR=/run/user/1000 WAYLAND_DISPLAY=wayland-0 grim ${remote}`]);
 await run('scp',['-O',...flags,host+':'+remote,local]);
 const {stdout}=await run('python3',['-c','from PIL import Image; import sys,json; im=Image.open(sys.argv[1]).convert("RGB"); print(json.dumps({"pointer":im.getpixel((400,400)),"arrowFill":(255,232,76) in im.crop((400,400,432,432)).getdata(),"label":im.getpixel((20,im.height-20)),"size":im.size}))',local]);
 return JSON.parse(stdout);
}
const background=[17,34,51],white=[255,255,255];
try {
 await c.call('settings.set',{tombstone:'off',durationSeconds:null});
 const receipt=await c.call('playback.replace',{playlist:{id:'static-presentation-test',items:[{source:'http://127.0.0.1:18990/presentation.html',duration:0,title:'Native label',metadata:{artists:['Test fixture']}}]}});
 let ready=false;
 for(let i=0;i<100;i++){const s=await c.call('state.get');if(s.presented?.id===receipt.occurrence){ready=true;break;}await delay(100);}
 assert(ready,'static artwork not presented');await delay(5500);
 assert.deepEqual((await capture('initial')).pointer,background);
 await c.call('input.dispatch',{occurrence:receipt.occurrence,kind:'move',x:400,y:400});await delay(300);
 const pointer=await capture('pointer');
 assert.deepEqual(pointer.pointer,[20,20,20]);
 assert.equal(pointer.arrowFill,true,'The yellow arrow fill must appear beside the dark hotspot');
 await delay(5200);assert.deepEqual((await capture('pointer-expired')).pointer,background);
 evidence.checks.push('cursor movement and five-second expiry redraw static artwork');
 await c.call('settings.set',{tombstone:'on'});await delay(300);
 assert.deepEqual((await capture('label')).label,white);
 await c.call('settings.set',{tombstone:'off'});await delay(300);
 assert.deepEqual((await capture('label-hidden')).label,background);
 evidence.checks.push('label settings redraw static artwork');
 await c.call('settings.set',{tombstone:'timed'});const timedAt=performance.now();await delay(300);
 assert.deepEqual((await capture('label-timed')).label,white);
 await delay(Math.max(0,31500-(performance.now()-timedAt)));
 assert.deepEqual((await capture('label-expired')).label,background);
 evidence.checks.push('thirty-second label expiry redraws static artwork');
 evidence.result='passed';
} catch(error) {evidence.result='failed';evidence.error=String(error);process.exitCode=1;}
finally {c.close();await fs.writeFile(path.join(output,'result.json'),JSON.stringify(evidence,null,2)+'\n');console.log(JSON.stringify(evidence));}
