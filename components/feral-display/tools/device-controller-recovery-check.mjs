// Crash only the fixed fixture in an isolated test host, then exercise Go refresh.
// CDP is used for failure injection, never as the product command transport.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {connectNative} from './native-client.mjs';
import {diagnosticSession} from './diagnostic-cdp.mjs';
const [socket,endpoint,host,output]=process.argv.slice(2);
if(!socket||!endpoint||!host||!output)throw new Error('usage: device-controller-recovery-check.mjs SOCKET CDP-URL SSH-HOST OUTPUT');
const evidence={startedAt:new Date().toISOString(),physicalOutputVerified:false,crashes:[]};
const client=await connectNative(socket,'controller-recovery-fixture');
const source='http://127.0.0.1:18990/input.html';
async function wait(predicate){
 for(let i=0;i<300;i++){
  const state=await client.call('state.get');if(predicate(state))return state;
  await new Promise(resolve=>setTimeout(resolve,100));
 }
 throw Error('Native state did not reach expected failure/recovery phase');
}
try{
 const receipt=await client.call('playback.replace',{playlist:{id:'controld-refresh-test',items:[{source,duration:0}]}});
 let state=await wait(s=>s.presented?.id===receipt.occurrence);
 const instance=state.instance,generation=state.generation;
 for(let attempt=0;attempt<4;attempt++){
  const occurrence=state.presented.id;
  const targets=await(await fetch(endpoint+'/json')).json();
  const pages=targets.filter(t=>t.type==='page'&&t.url===source);
  assert.equal(pages.length,1,'Failure injection requires exactly one isolated fixture page');
  const version=await(await fetch(endpoint+'/json/version')).json();
  const address=new URL(version.webSocketDebuggerUrl);address.host=new URL(endpoint).host;
  const diagnostic=await diagnosticSession(address.href);
  let processes;
  try{processes=(await diagnostic.call('SystemInfo.getProcessInfo',{})).processInfo;}
  finally{diagnostic.close();}
  const browser=processes.find(p=>p.type==='browser');
  const renderers=processes.filter(p=>p.type==='renderer').map(p=>p.id);
  assert.ok(browser&&Number.isInteger(browser.id));assert.ok(renderers.length>0&&renderers.every(Number.isInteger));
  const helper=await fs.readFile(new URL('./kill-test-renderers.py',import.meta.url));
  const killed=await new Promise((resolve,reject)=>{
   const flags=['-o','ConnectTimeout=8','-o','IPQoS=none','-o','BatchMode=yes','-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts','-i',process.env.HOME+'/.ssh/id_ed25519'];
   const child=spawn('ssh',[...flags,host,'python3','-','ff-native-runtime-test.service',String(browser.id),...renderers.map(String)]);
   let log='';child.stdout.on('data',data=>log+=data);child.stderr.on('data',data=>log+=data);child.on('error',reject);
   child.on('close',code=>code===0?resolve(JSON.parse(log)):reject(new Error(log)));child.stdin.end(helper);
  });
  state=await wait(s=>s.selected?.id===occurrence&&s.renderer==='failed');
  assert.equal(state.instance,instance);assert.equal(state.generation,generation);
  evidence.crashes.push({occurrence,killed,recovery:state.recovery,error:state.selected.error});
  if(attempt<3){state=await wait(s=>s.presented?.id&&s.presented.id!==occurrence);}
 }
 assert.equal(state.recovery?.exhausted,true);evidence.failed=state;
 client.close();
 await new Promise((resolve,reject)=>{
  const child=spawn('go',['test','./commandrouter','-run','^TestNativeRefreshFailedDevice$','-count=1','-v'],{cwd:fileURLToPath(new URL('../../feral-controld/',import.meta.url)),env:{...process.env,FF_NATIVE_CONTROLS_TEST_SOCKET:socket}});
  let log='';child.stdout.on('data',data=>log+=data);child.stderr.on('data',data=>log+=data);child.on('error',reject);
  child.on('close',code=>{evidence.goTest=log;code===0?resolve():reject(new Error(log));});
 });
 evidence.result='passed';
}catch(error){evidence.result='failed';evidence.error=String(error);process.exitCode=1;}
finally{client.close();evidence.finishedAt=new Date().toISOString();await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');}
console.log(evidence.result);
