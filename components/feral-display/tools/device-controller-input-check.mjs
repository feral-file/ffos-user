import {publicDiagnostic} from './public-diagnostic.mjs';
// Exercise the real Go gesture adapter over the socket; CDP observes the fixture.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {connectNative} from './native-client.mjs';
import {diagnosticSession} from './diagnostic-cdp.mjs';
let [socket,endpoint,output]=process.argv.slice(2);
if(!socket||!endpoint||!output)throw new Error('usage: device-controller-input-check.mjs SOCKET CDP-URL OUTPUT');
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const fixture='data:text/html;base64,'+(await fs.readFile(new URL('../tests/input.html',import.meta.url))).toString('base64');
const evidence={startedAt:new Date().toISOString(),physicalOutputVerified:false,checks:[]};
const client=await connectNative(socket,'controller-input-fixture');let observer,diagnostic;
try{
 await client.call('connectivity.set',{online:true});
 const receipt=await client.call('playback.replace',{playlist:{id:'controld-input-test',items:[{source:fixture,duration:0,display:{margin:40,userOverrides:false}}]}});
 let state;
 for(let i=0;i<150;i++){state=await client.call('state.get');if(state.presented?.id===receipt.occurrence)break;await delay(100);}
 assert.equal(state.presented?.id,receipt.occurrence);
 if(endpoint==='public'){diagnostic=await publicDiagnostic(receipt.occurrence,state.media.browserPid);endpoint=diagnostic.endpoint;}
 await client.call('input.dispatch',{occurrence:receipt.occurrence,kind:'move',x:200,y:200});
 client.close();await delay(200);
 const targets=await(await fetch(`${endpoint}/json`)).json();
 const target=targets.find(t=>t.type==='page'&&t.url===fixture);assert.ok(target);
 const address=new URL(target.webSocketDebuggerUrl);address.host=new URL(endpoint).host;
 observer=await diagnosticSession(address.href);
 await new Promise((resolve,reject)=>{
  const child=spawn('go',['test','./devicectl','-run','^TestNativeInputDevice$','-count=1','-v'],{cwd:fileURLToPath(new URL('../../feral-controld/',import.meta.url)),env:{...process.env,FF_NATIVE_INPUT_TEST_SOCKET:socket}});
  let log='';child.stdout.on('data',data=>log+=data);child.stderr.on('data',data=>log+=data);child.on('error',reject);
  child.on('close',code=>{evidence.goTest=log;code===0?resolve():reject(new Error(log));});
 });
 await delay(300);
 const observed=await observer.call('Runtime.evaluate',{expression:'({events:window.inputEvents,text:document.querySelector("#text").value,width:innerWidth,height:innerHeight,formText:document.querySelector("#formText").value,formSubmits:window.formSubmits})',returnByValue:true});
 const data=observed.result.value;evidence.observation=data;
 const move=data.events.find(e=>e.type==='mousemove'&&e.buttons===1);
 assert.ok(move);const scale=(data.width+80)/1920;
 assert.equal(move.x,Math.floor(200+10*scale)-40);assert.equal(move.y,Math.floor(200+5*scale)-40);
 evidence.checks.push('relative gesture and visible host pointer use current 4K geometry');
 assert.ok(data.events.some(e=>e.type==='dblclick'&&e.detail===2));evidence.checks.push('two-click sequence produces DOM dblclick');
 const wheel=data.events.filter(e=>e.type==='wheel');assert.equal(wheel.length,2);assert.ok(wheel[0].deltaY<0&&wheel[1].deltaY>0);assert.equal(Math.abs(wheel[0].deltaY),960);assert.equal(wheel[1].deltaY,960);
 evidence.checks.push('pinch-in/out preserves DOM wheel direction');
 assert.ok(data.events.some(e=>e.type==='keydown'&&e.code==='KeyA'&&e.shift));
 assert.ok(data.events.some(e=>e.type==='keydown'&&e.code==='Digit5'&&e.shift));
 assert.equal(data.text,'A%\n');assert.equal(data.formText,'A');assert.equal(data.formSubmits,1);evidence.checks.push('ASCII text, Shift physical keys and Enter default editing');
 evidence.result='passed';
}finally{client.close();observer?.close();diagnostic?.close();evidence.finishedAt=new Date().toISOString();await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');}
console.log(`${evidence.checks.length} controld/native input checks passed`);
