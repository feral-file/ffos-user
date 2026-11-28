import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
import {connectNative} from './native-client.mjs';
import {diagnosticSession} from './diagnostic-cdp.mjs';
const [socketPath,diagnosticURL,outputDirectory]=process.argv.slice(2);
if(!outputDirectory)throw new Error('usage: device-image-check.mjs SOCKET CDP-URL OUTPUT-DIRECTORY');
await fs.mkdir(outputDirectory,{recursive:true});
const fixtures=JSON.parse(await fs.readFile(new URL('../tests/image-fixtures.json',import.meta.url),'utf8'));
const client=await connectNative(socketPath,'image-check');
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const evidence={startedAt:new Date().toISOString(),checks:[]};
let observer;
try{
 await client.call('settings.set',{durationSeconds:null,framing:'artwork'});
 for(const fixture of fixtures){
  observer?.close();observer=null;
  const receipt=await client.call('playback.replace',{playlist:{items:[{...fixture,id:fixture.name,title:fixture.name,duration:0,display:{scaling:'fit',margin:0}}]}});
  let state;
  for(let i=0;i<150;i++){
   state=await client.call('state.get');if(state.presented?.id===receipt.occurrence)break;
   if(state.renderer==='failed')throw new Error(state.selected.error);await delay(100);
  }
  assert.equal(state.presented?.id,receipt.occurrence);
  const targets=await(await fetch(`${diagnosticURL}/json`)).json();
  const target=targets.find(t=>t.url==='ff-render://bundle/index.html'&&t.type==='page');assert.ok(target);
  const address=new URL(target.webSocketDebuggerUrl);address.host=new URL(diagnosticURL).host;
  observer=await diagnosticSession(address.href);
  const observed=await observer.call('Runtime.evaluate',{expression:'(() => {const i=document.querySelector("img");return {size:[i.naturalWidth,i.naturalHeight],complete:i.complete,fit:getComputedStyle(i).objectFit,width:innerWidth,height:innerHeight};})()',returnByValue:true});
  const data=observed.result.value;assert.deepEqual(data.size,fixture.expectedSize);assert.equal(data.complete,true);assert.equal(data.fit,'contain');
  for(let sample=0;sample<(fixture.name==='animated-gif'?6:1);sample++){
   const screenshot=await observer.call('Page.captureScreenshot',{format:'png'});
   await fs.writeFile(path.join(outputDirectory,`${fixture.name}-${sample}.png`),Buffer.from(screenshot.data,'base64'));
   await delay(210);
  }
  evidence.checks.push({name:fixture.name,occurrence:receipt.occurrence,observed:data});
 }
 evidence.result='dimensions_passed_pixel_validation_pending';
}catch(error){evidence.result='failed';evidence.error=String(error);throw error;}
finally{
 observer?.close();client.close();evidence.finishedAt=new Date().toISOString();
 await fs.writeFile(path.join(outputDirectory,'result.json'),JSON.stringify(evidence,null,2)+'\n');
}
