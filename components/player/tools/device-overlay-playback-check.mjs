// Run against an isolated player. Covers setup over live art and media selected
// behind setup, where navigation, a decoded frame and presentation differ.
import assert from 'node:assert/strict';
import {writeFile} from 'node:fs/promises';
import {connectNative} from './native-client.mjs';
const [socket,endpoint,output]=process.argv.slice(2);
assert(output,'usage: device-overlay-playback-check.mjs SOCKET CDP-HTTP OUTPUT');
const c=await connectNative(socket,'overlay-playback-regression');
const delay=ms=>new Promise(r=>setTimeout(r,ms));
const evidence={checks:[],physicalOutputVerified:false,startedAt:new Date().toISOString()};
async function waitFor(predicate,timeout=15000){
 const until=performance.now()+timeout;
 while(performance.now()<until){const s=await c.call('state.get');assert.notEqual(s.selected?.stage,'failed',JSON.stringify(s.selected));if(predicate(s))return s;await delay(100);}
 throw Error('Timed out waiting for native state');
}
async function backdrops(){return (await (await fetch(endpoint+'/json')).json()).filter(x=>x.url.includes('/setup-artwork/'));}
try{
 await c.call('overlay.set',{owner:'setup',state:'ready'});
 await c.call('playback.stop');await c.call('settings.set',{muted:true,durationSeconds:null});
 await c.call('playback.mode',{loop:'playlist'});
 let receipt=await c.call('playback.replace',{playlist:{items:[{source:'http://127.0.0.1:18990/presentation.html',duration:0}]}});
 await waitFor(s=>s.presented?.id===receipt.occurrence);
 await c.call('overlay.set',{owner:'setup',state:'joining'});await delay(1500);
 assert.equal((await backdrops()).length,0,'Live artwork must remain the setup backdrop');
 assert.equal((await c.call('state.get')).presented.id,receipt.occurrence);
 evidence.checks.push('setup over presented artwork creates no decorative renderer');
 await c.call('playback.stop');
 const until=performance.now()+15000;while(!(await backdrops()).length&&performance.now()<until)await delay(100);
 assert.equal((await backdrops()).length,1,'Empty setup must have one bundled backdrop');
 receipt=await c.call('playback.replace',{playlist:{items:[
  {source:'http://127.0.0.1:18990/media-h264-aac.mp4',duration:0,display:{loop:false}},
  {source:'http://127.0.0.1:18990/presentation.html',duration:0}
 ]}});
 await waitFor(s=>s.selected?.stage==='committed'&&(s.media?.importedFrames||0)>0);
 // Beyond both the four-second fixture duration and the 60-second load limit.
 const end=performance.now()+65000;
 while(performance.now()<end){const s=await c.call('state.get');assert.equal(s.selected.id,receipt.occurrence);assert.equal(s.selected.stage,'committed');assert.equal(s.presented,null);assert.equal(s.media.pause,true);assert((s.media['time-pos']||0)<.15);await delay(500);}
 evidence.checks.push('covered video keeps first frame paused for 65 seconds without false presentation or timeout');
 await c.call('overlay.set',{owner:'setup',state:'ready'});
 await waitFor(s=>s.presented?.id===receipt.occurrence);
 await waitFor(s=>s.presented?.index===1);
 assert.equal((await backdrops()).length,0);
 evidence.checks.push('uncover starts video, then EOF advances to next occurrence and retires backdrop');
 evidence.result='passed';
}catch(error){evidence.result='failed';evidence.error=String(error);process.exitCode=1;}
finally{await c.call('overlay.set',{owner:'setup',state:'ready'}).catch(()=>{});c.close();evidence.finishedAt=new Date().toISOString();await writeFile(output,JSON.stringify(evidence,null,2)+'\n');console.log(JSON.stringify(evidence));}
