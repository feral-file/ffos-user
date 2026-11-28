// Native control with read-only renderer diagnostics. Screenshots are renderer
// output, not proof of the physical panel or visual equivalence to ff-player.
import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
import {connectNative} from './native-client.mjs';
import {diagnosticSession} from './diagnostic-cdp.mjs';

const [socketPath, diagnosticURL, outputDirectory] = process.argv.slice(2);
if(!outputDirectory) throw new Error('usage: device-model-check.mjs SOCKET CDP-URL OUTPUT-DIR');
await fs.mkdir(outputDirectory,{recursive:true});
const client=await connectNative(socketPath,'model-check');
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const evidence={startedAt:new Date().toISOString(),checks:[]};
const fixtureBase=process.env.FF_MODEL_FIXTURE_BASE || 'https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Assets/f0920f5b32d172a30efe3f9257e965918831e6ea/Models/';
evidence.fixtureBase=fixtureBase;
let observer;
const models=[
  ['box','Box/glTF-Binary/Box.glb'],
  ['draco','Box/glTF-Draco/Box.gltf'],
  ['draco-ktx2','ABeautifulGame/glTF-Binary-KTX-ETC1S-Draco/ABeautifulGame.glb'],
];
try{
  for(const [name,model] of models){
    observer?.close();observer=undefined;
    const receipt=await client.call('playback.replace',{playlist:{items:[{source:new URL(model,fixtureBase).href,duration:0}]}});
    // Exercise configuration arriving during asynchronous viewer bootstrap.
    for(let i=0;i<5;i++){await client.call('settings.set',{volume:i%2?0.25:0.5});await delay(30);}
    let state;
    for(let i=0;i<300;i++){
      state=await client.call('state.get');
      if(state.presented?.id===receipt.occurrence)break;
      if(state.renderer==='failed')throw new Error(state.selected.error);
      await delay(100);
    }
    assert.equal(state.presented?.id,receipt.occurrence,'model must produce host presentation');
    const targets=await(await fetch(`${diagnosticURL}/json`)).json();
    const target=targets.find(t=>t.url==='ff-render://bundle/index.html'&&t.type==='page');
    assert.ok(target,'bundled renderer diagnostic target');
    const url=new URL(target.webSocketDebuggerUrl);url.host=new URL(diagnosticURL).host;
    observer=await diagnosticSession(url.href);
    const observed=await observer.call('Runtime.evaluate',{expression:`(() => {
      const m=document.querySelector('model-viewer');
      return {loaded:m?.loaded,attributes:Object.fromEntries([...m.attributes].map(a=>[a.name,a.value])),
        camera:m?.getCameraOrbit().toString(),resources:performance.getEntriesByType('resource').map(r=>r.name),
        draco:customElements.get('model-viewer').dracoDecoderLocation,
        basis:customElements.get('model-viewer').ktx2TranscoderLocation};
    })()`,returnByValue:true});
    const data=observed.result.value;assert.equal(data.loaded,true);
    assert.equal(data.attributes.exposure,'1');assert.equal(data.attributes['shadow-intensity'],'1');
    assert.ok(Object.hasOwn(data.attributes,'autoplay'));assert.ok(Object.hasOwn(data.attributes,'camera-controls'));
    assert.ok(data.resources.every(r=>r.startsWith('ff-render://')||r.startsWith(fixtureBase)||r.startsWith('blob:')||r.startsWith('data:')),'no decoder CDN request');
    // CEF's custom-scheme handler does not populate Resource Timing entries.
    // Its decoder opens are recorded separately in the host's diagnostic log.
    assert.equal(data.draco,'ff-render://bundle/draco/');
    assert.equal(data.basis,'ff-render://bundle/basis/');
    await delay(500);
    const screenshot=await observer.call('Page.captureScreenshot',{format:'png'});
    await fs.writeFile(path.join(outputDirectory,`${name}.png`),Buffer.from(screenshot.data,'base64'));
    const occurrence=state.presented.id;
    await client.call('input.dispatch',{occurrence,kind:'move',x:900,y:900});
    await client.call('input.dispatch',{occurrence,kind:'button',button:1,down:true});
    for(let i=0;i<8;i++){await client.call('input.dispatch',{occurrence,kind:'move',dx:25,dy:0});await delay(30);}
    await client.call('input.dispatch',{occurrence,kind:'button',button:1,down:false});
    await delay(500);
    const moved=await observer.call('Runtime.evaluate',{expression:'document.querySelector("model-viewer").getCameraOrbit().toString()',returnByValue:true});
    assert.notEqual(moved.result.value,data.camera,'native dragging must move the model camera');
    evidence.checks.push({name,occurrence,observed:data,cameraAfterDrag:moved.result.value});
  }
  evidence.result='passed';
}catch(error){evidence.result='failed';evidence.error=String(error);throw error;}
finally{
  observer?.close();client.close();evidence.finishedAt=new Date().toISOString();
  await fs.writeFile(path.join(outputDirectory,'result.json'),JSON.stringify(evidence,null,2)+'\n');
}
console.log(`${evidence.checks.length} model decode/input checks passed`);
