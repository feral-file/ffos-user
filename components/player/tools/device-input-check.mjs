// Product input travels over the native socket. CDP is read-only test observation.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import {diagnosticSession} from './diagnostic-cdp.mjs';

const [socketPath, diagnosticURL, output] = process.argv.slice(2);
if (!socketPath || !diagnosticURL || !output) throw new Error('usage: node device-input-check.mjs SOCKET CDP-URL OUTPUT');
import {connectNative} from './native-client.mjs';
const connect = () => connectNative(socketPath, 'native-input-check');
const delay = ms => new Promise(resolve=>setTimeout(resolve,ms));
const evidence={startedAt:new Date().toISOString(),checks:[]};
let client=await connect(),observer;
async function cast(){
  const receipt=await client.call('playback.replace',{playlist:{id:'input-test',title:'Native input verification',items:[
    {source:'http://127.0.0.1:18990/input.html',duration:0,display:{margin:40,userOverrides:false}}]}});
  for(let count=0;count<150;count++){
    const state=await client.call('state.get');
    if(state.presented?.id===receipt.occurrence){
      await delay(300);
      const targets=await (await fetch(`${diagnosticURL}/json`)).json();
      const target=targets.find(t=>t.url==='http://127.0.0.1:18990/input.html'&&t.type==='page');
      assert.ok(target,'diagnostic artwork target exists');
      const ws=new URL(target.webSocketDebuggerUrl),endpoint=new URL(diagnosticURL);ws.host=endpoint.host;
      observer=await diagnosticSession(ws.href);return state.presented.id;
    }
    if(state.renderer==='failed')throw new Error(state.selected.error);
    await delay(100);
  }
  throw new Error('input fixture was not presented');
}
async function observed(){
  await delay(120);
  const result=await observer.call('Runtime.evaluate',{expression:'({events:window.inputEvents,text:document.querySelector("#text").value})',returnByValue:true});
  return result.result.value;
}
let occurrence;
const input=params=>client.call('input.dispatch',{occurrence,...params});
try {
  occurrence=await cast();
  await input({kind:'move',x:200,y:200});
  await input({kind:'button',button:1,down:true});
  await input({kind:'move',dx:40,dy:50});
  await input({kind:'button',button:1,down:false});
  let data=await observed();
  assert.ok(data.events.some(e=>e.type==='mousemove'&&e.x===200&&e.y===210&&e.buttons===1),JSON.stringify(data));
  evidence.checks.push({name:'drag uses output pixels minus 40-pixel matte',events:data.events});
  await input({kind:'button',button:1,down:true,clicks:2});
  await input({kind:'button',button:1,down:false,clicks:2});
  await input({kind:'wheel',dx:0,dy:-40});
  data=await observed();
  assert.ok(data.events.some(e=>e.type==='dblclick'));
  assert.ok(data.events.some(e=>e.type==='wheel'));
  evidence.checks.push({name:'double-click and scroll reach artwork'});
  await input({kind:'key',usage:225,down:true});
  await input({kind:'key',usage:4,down:true});
  await input({kind:'text',text:'A'});
  await input({kind:'key',usage:4,down:false});
  await input({kind:'key',usage:225,down:false});
  await input({kind:'text',text:'é猫'});
  await input({kind:'key',usage:80,down:true});
  await input({kind:'key',usage:80,down:false});
  data=await observed();
  assert.ok(data.events.some(e=>e.type==='keydown'&&e.code==='KeyA'&&e.key==='A'&&e.shift));
  assert.ok(data.events.some(e=>e.type==='keydown'&&e.code==='ArrowLeft'&&e.key==='ArrowLeft'));
  assert.equal(data.text,'Aé猫');
  evidence.checks.push({name:'physical keys, modifiers and Unicode text remain separate',events:data.events,text:data.text});
  await input({kind:'key',usage:225,down:true});
  await input({kind:'button',button:1,down:true});
  client.close();await delay(300);data=await observed();
  assert.ok(data.events.slice(-8).some(e=>e.type==='keyup'&&e.code==='ShiftLeft'));
  assert.ok(data.events.slice(-8).some(e=>e.type==='mouseup'&&e.buttons===0));
  evidence.checks.push({name:'disconnect releases held key and button',events:data.events.slice(-8)});
  client=await connect();observer.close();const old=occurrence;occurrence=await cast();
  await assert.rejects(client.call('input.dispatch',{occurrence:old,kind:'button',down:true}),{code:'stale_occurrence'});
  await input({kind:'button',down:true});await input({kind:'button',down:false});data=await observed();
  assert.ok(data.events.some(e=>e.type==='mousedown'&&e.x===200&&e.y===210));
  evidence.checks.push({name:'same-URL occurrence rejects stale input and retains authoritative pointer',events:data.events});
} finally {
  client.close();observer?.close();evidence.finishedAt=new Date().toISOString();
  await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');
}
console.log(`${evidence.checks.length} native input checks passed`);
