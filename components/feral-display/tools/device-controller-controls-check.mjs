// Real Go controller translation against an isolated native host. No CDP writes.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {connectNative} from './native-client.mjs';
const [socket,output]=process.argv.slice(2);
if(!socket||!output)throw new Error('usage: device-controller-controls-check.mjs SOCKET OUTPUT');
const evidence={startedAt:new Date().toISOString(),physicalOutputVerified:false};
const client=await connectNative(socket,'controller-controls-fixture');
try{
 await client.call('settings.set',{framing:'artwork',tombstone:'off',durationSeconds:null});
 const item={source:'http://127.0.0.1:18990/input.html',duration:0};
 const receipt=await client.call('playback.replace',{playlist:{id:'controld-controls-test',items:[{...item,title:'First'},{...item,title:'Second'}]}});
 for(let i=0;i<150;i++){
  evidence.before=await client.call('state.get');
  if(evidence.before.presented?.id===receipt.occurrence)break;
  await new Promise(resolve=>setTimeout(resolve,100));
 }
 assert.equal(evidence.before.presented?.id,receipt.occurrence);
 client.close();
 await new Promise((resolve,reject)=>{
  const child=spawn('go',['test','./commandrouter','-run','^TestNativeControlsDevice$','-count=1','-v'],{cwd:fileURLToPath(new URL('../../feral-controld/',import.meta.url)),env:{...process.env,FF_NATIVE_CONTROLS_TEST_SOCKET:socket}});
  let log='';child.stdout.on('data',data=>log+=data);child.stderr.on('data',data=>log+=data);child.on('error',reject);
  child.on('close',code=>{evidence.goTest=log;code===0?resolve():reject(new Error(log));});
 });
 const after=await connectNative(socket,'controller-controls-result');
 try{evidence.after=await after.call('state.get');}finally{after.close();}
 evidence.result='passed';
}catch(error){evidence.result='failed';evidence.error=String(error);process.exitCode=1;}
finally{client.close();evidence.finishedAt=new Date().toISOString();await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');}
console.log(evidence.result);
