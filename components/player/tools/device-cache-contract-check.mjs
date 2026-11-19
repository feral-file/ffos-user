// Original-URL cache integration. A renderer capture is not physical-panel proof.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import {connectNative} from './native-client.mjs';
import {publicDiagnostic} from './public-diagnostic.mjs';
import {diagnosticSession} from './diagnostic-cdp.mjs';
const [socket,output]=process.argv.slice(2);assert(socket&&output);
const c=await connectNative(socket);const evidence={startedAt:new Date().toISOString(),physicalOutputVerified:false};let tunnel,observer;
try{
 await c.call('connectivity.set',{online:!process.env.FF_OFFLINE_COLD});
 await c.call('settings.set',{tombstone:'off',durationSeconds:null});
 if(!process.env.FF_OFFLINE_COLD)await c.call('playback.replace',{playlist:{id:'offline-contract',items:[{source:'http://127.0.0.1:18990/offline-contract.html',duration:0}]}});
 let state;
 for(let i=0;i<200;i++){state=await c.call('state.get');if(state.selected?.stage==='failed')throw Error(state.selected.error);if(state.presented?.id===state.selected?.id&&state.media?.backend==='public-chromium'&&state.presented?.item?.source==='http://127.0.0.1:18990/offline-contract.html')break;await new Promise(r=>setTimeout(r,100));}
 assert.equal(state.presented?.item?.source,'http://127.0.0.1:18990/offline-contract.html');
 tunnel=await publicDiagnostic(state.presented.id,state.media.browserPid);const targets=await(await fetch(tunnel.endpoint+'/json')).json();const target=targets.find(t=>t.type==='page'&&t.url===state.presented.item.source);assert(target);let address=new URL(target.webSocketDebuggerUrl);address.host=new URL(tunnel.endpoint).host;observer=await diagnosticSession(address.href);
 let results;
 for(let i=0;i<100;i++){results=(await observer.call('Runtime.evaluate',{expression:'window.results',returnByValue:true})).result.value;if(results?.font&&results.wasm===42&&results.frame&&results.worker)break;await new Promise(r=>setTimeout(r,100));}
 assert.equal(results.font,true);assert.equal(results.wasm,42);assert.equal(results.frame,'same-origin');assert.equal(results.redirect,'original-response');assert.equal(results.runtimeFetch,'original-response');assert.equal(results.worker,'worker-runtime-success');
 await new Promise(r=>setTimeout(r,1100));state=await c.call('state.get');assert.equal(state.media.offline,true);assert.ok(state.media.cacheHits>=7);assert.ok(state.media.unsupportedWorkerTargets>=1);assert.equal(state.media.captureCoverageComplete,false);assert.ok(state.notice||state.runtimeNotice,'Incomplete worker dependencies must reach the sender');
 evidence.result='passed_with_explicit_service_worker_limitation';evidence.results=results;evidence.media=state.media;evidence.notice=state.notice||state.runtimeNotice;evidence.engine=state.engine;evidence.instance=state.instance;evidence.occurrence=state.presented.id;evidence.online=state.online;
}catch(error){evidence.result='failed';evidence.error=String(error);throw error;}finally{observer?.close();tunnel?.close();c.close();await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');}
console.log(JSON.stringify(evidence));
