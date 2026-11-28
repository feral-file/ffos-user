// Native socket control; no page evaluation or CDP product dependency.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import {connectNative} from './native-client.mjs';

const [socketPath,output]=process.argv.slice(2);
if(!output)throw new Error('usage: device-refresh-check.mjs SOCKET OUTPUT');
const client=await connectNative(socketPath,'refresh-check');
const evidence={startedAt:new Date().toISOString(),checks:[]};
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
async function wait(predicate){
  let state;
  for(let i=0;i<200;i++){
    state=await client.call('state.get');if(predicate(state))return state;
    if(state.renderer==='failed')throw new Error(state.selected.error);
    await delay(50);
  }
  throw new Error(`state did not settle: ${JSON.stringify(state)}`);
}
const media={id:'twin',source:'http://127.0.0.1:18990/media-h264-aac.mp4',duration:0,contentRating:'general',display:{loop:false,autoplay:false}};
const software={id:'software',source:'http://127.0.0.1:18990/input.html',duration:0,contentRating:'general'};
try{
  await client.call('settings.set',{durationSeconds:null,muted:true});
  await client.call('policy.set',{version:1,showMatureContent:false,strictPersonal:false,blockUnratedCurated:false});
  const receipt=await client.call('playback.replace',{playlist:{id:'refresh-device',signature:'synthetic',items:[media,media]},index:1});
  let state=await wait(s=>s.presented?.id===receipt.occurrence&&(s.media||{}).workerPid);
  const occurrence=state.presented.id,worker=state.media.workerPid;
  const history=await client.call('history.list');
  await client.call('playback.update',{items:[software,media,{...media,title:'Second duplicate slot'}]});
  state=await client.call('state.get');
  assert.equal(state.presented.id,occurrence);assert.equal(state.presented.index,2);
  assert.equal(state.media.workerPid,worker);assert.equal(state.presented.item.title,'Second duplicate slot');
  assert.equal(state.presentedPlaylist.signature,undefined);
  assert.equal((await client.call('history.list')).records.length,history.records.length);
  evidence.checks.push({name:'duplicate slot refresh retains decoded occurrence and history',occurrence,worker,index:state.presented.index});
  const update=await client.call('playback.update',{items:[software]});
  state=await client.call('state.get');
  assert.equal(state.pendingUpdate.id,update.updateId);assert.equal(state.presented.id,occurrence);
  assert.ok(state.selected.generation<state.generation);
  await client.call('media.seek',{occurrence,seconds:3.5});
  await client.call('playback.pause',{paused:false});
  state=await wait(s=>s.presented?.item.id==='software');
  assert.equal(state.pendingUpdate,null);assert.equal(state.updateOutcome.id,update.updateId);
  assert.equal(state.updateOutcome.outcome,'applied');assert.notEqual(state.presented.id,occurrence);
  evidence.checks.push({name:'native media EOF applies deferred software playlist',updateId:update.updateId,occurrence:state.presented.id});
  const policyUpdate=await client.call('playback.update',{items:[{...software,contentRating:'mature'}]});
  state=await client.call('state.get');assert.equal(state.presented,null);assert.equal(state.selected,null);
  assert.equal(state.updateOutcome.contentBlocked,true);assert.equal(state.updateOutcome.id,policyUpdate.updateId);
  evidence.checks.push({name:'fresh blocked labels retire presentation and expose outcome',updateId:policyUpdate.updateId});
  evidence.result='passed';
}catch(error){evidence.result='failed';evidence.error=String(error);throw error;}
finally{
  client.close();evidence.finishedAt=new Date().toISOString();
  await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');
}
console.log(`${evidence.checks.length} native playlist refresh checks passed`);
