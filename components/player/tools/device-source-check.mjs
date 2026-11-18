// Backend selection for extensionless URLs, without changing the authored item.
import assert from 'node:assert/strict';
import {writeFile} from 'node:fs/promises';
import {connectNative} from './native-client.mjs';
const [socket,output]=process.argv.slice(2);assert(output,'usage: device-source-check.mjs SOCKET OUTPUT');
const c=await connectNative(socket,'source-classification-check');
const evidence={checks:[],physicalOutputVerified:false};
const delay=ms=>new Promise(r=>setTimeout(r,ms));
try{
 await c.call('overlay.set',{owner:'setup',state:'ready'});
 await c.call('settings.set',{muted:true,durationSeconds:null});
 for(const path of ['opaque-video','opaque-video-head-rejected','opaque-redirect']){
  const item={source:'http://127.0.0.1:18990/'+path,duration:0,display:{loop:true}};
  const receipt=await c.call('playback.replace',{playlist:{items:[item]}});
  let state;
  for(let i=0;i<150;i++){
   state=await c.call('state.get');assert.notEqual(state.renderer,'failed',state.selected?.error);
   if(state.presented?.id===receipt.occurrence&&state.media?.['video-codec']&&state.media?.['time-pos']>.3)break;
   await delay(100);
  }
  assert.equal(state.presented?.id,receipt.occurrence);assert(state.media?.['time-pos']>.3);
  assert.deepEqual(state.selected.item,item);assert.deepEqual(state.playlist.items,[item]);
  evidence.checks.push({path,decoder:state.media['video-codec'],hwdec:state.media['hwdec-current'],frames:state.media.importedFrames});
 }
 const slow={playlist:{items:[{source:'http://127.0.0.1:18990/opaque-slow',duration:0}]}};
 await c.call('playback.replace',slow);await delay(100);
 const replacement=await c.call('playback.replace',{playlist:{items:[{source:'http://127.0.0.1:18990/presentation.html',duration:0}]}});
 await delay(6000);
 let state=await c.call('state.get');
 assert.equal(state.presented?.id,replacement.occurrence);assert.equal(state.selected?.id,replacement.occurrence);
 evidence.checks.push({name:'superseded slow probe cannot replace the newer artwork'});
 const started=performance.now(),timeout=await c.call('playback.replace',slow);
 while(performance.now()-started<5000){state=await c.call('state.get');if(state.selected?.stage==='failed')break;await delay(50);}
 assert.equal(state.selected?.id,timeout.occurrence);assert.equal(state.selected.stage,'failed');assert.equal(state.host,'responsive');
 assert.notEqual(state.presented?.id,timeout.occurrence);
 evidence.checks.push({name:'probe timeout produces an incoming failure, not false presentation',elapsedMs:performance.now()-started});
 await c.call('playback.stop');
 evidence.result='passed';
}catch(error){evidence.result='failed';evidence.error=String(error);process.exitCode=1;}
finally{c.close();await writeFile(output,JSON.stringify(evidence,null,2)+'\n');console.log(JSON.stringify(evidence));}
