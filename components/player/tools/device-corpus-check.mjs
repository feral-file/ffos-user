// Portfolio observations, never a visual-correctness verdict. All selection
// travels through the native socket; CDP is read-only diagnostic evidence.
import fs from 'node:fs/promises';
import path from 'node:path';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {connectNative} from './native-client.mjs';
import {diagnosticSession} from './diagnostic-cdp.mjs';
const execute=promisify(execFile);
const [socketPath,diagnosticURL,sshHost,outputDirectory]=process.argv.slice(2);
if(!outputDirectory)throw new Error('usage: device-corpus-check.mjs SOCKET CDP-URL SSH-HOST OUTPUT-DIRECTORY');
const corpus=JSON.parse(await fs.readFile(new URL('../tests/living-code.json',import.meta.url),'utf8'));
const subset=process.env.FF_CORPUS_INDEXES?.split(',').map(Number);
await fs.mkdir(outputDirectory,{recursive:true});
const client=await connectNative(socketPath,'corpus-check');
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const sshArgs=['-o','IPQoS=none','-i','/Users/anhnguyen/.ssh/id_ed25519','-o','BatchMode=yes','-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts'];
const evidence={source:corpus.source,blobSHA:corpus.blobSHA,startedAt:new Date().toISOString(),checks:[],physicalDisplayVerified:false,visualReviewComplete:false};
const targets=async()=>await(await fetch(`${diagnosticURL}/json`,{signal:AbortSignal.timeout(5000)})).json();
let observer;
try{
 await client.call('settings.set',{durationSeconds:null,tombstone:'on',muted:true});
 await client.call('connectivity.set',{online:true});
 for(let index=0;index<corpus.playlist.items.length;index++){
  if(subset&&!subset.includes(index))continue;
  observer?.close();observer=null;
  const work=corpus.playlist.items[index],stem=String(index).padStart(2,'0');
  const record={index,title:work.title,source:work.source,startedAt:new Date().toISOString(),responses:[],exceptions:[],loadingFailures:[]};
  evidence.checks.push(record);
  try{
   const previous=new Set((await targets()).map(t=>t.id));
   const item={...work,id:`corpus-${index}`,title:work.label.title,duration:0,metadata:{artists:[{name:work.label.artist}]}};
   const receipt=await client.call('playback.replace',{playlist:{id:'living-code-corpus',title:corpus.playlist.title,items:[item]}});
   record.receipt=receipt;
   for(let attempt=0;attempt<100;attempt++){
    const target=(await targets()).find(t=>t.type==='page'&&!previous.has(t.id));
    if(target){
     const address=new URL(target.webSocketDebuggerUrl);address.host=new URL(diagnosticURL).host;
     try{
     observer=await diagnosticSession(address.href);
     observer.observe('Network.responseReceived',e=>{if(record.responses.length<256)record.responses.push({url:e.response.url,status:e.response.status,type:e.type,headers:e.type==='Document'?e.response.headers:undefined});});
     observer.observe('Network.loadingFailed',e=>{if(record.loadingFailures.length<100)record.loadingFailures.push(e);});
     observer.observe('Runtime.exceptionThrown',e=>{if(record.exceptions.length<100)record.exceptions.push(e);});
     await observer.call('Network.enable',{});await observer.call('Runtime.enable',{});
     }catch(error){record.diagnosticError=String(error);observer?.close();observer=null;await delay(100);continue;}
     break;
    }
    await delay(50);
   }
   let state;
   for(let attempt=0;attempt<600;attempt++){
    state=await client.call('state.get');
    if(state.presented?.id===receipt.occurrence||state.selected?.stage==='failed')break;
    await delay(100);
   }
   record.initialState=state;
   if(state.presented?.id===receipt.occurrence){
    // Observe real activity after presentation. This duration is not a soak.
    await delay(10000);
    record.after=await client.call('state.get');
    if(observer){
     try{
     record.document=(await observer.call('Runtime.evaluate',{expression:'({url:location.href,title:document.title,readyState:document.readyState,topLevel:window===window.top,canvases:[...document.querySelectorAll("canvas")].map(c=>({width:c.width,height:c.height})),bodyText:document.body?.innerText?.slice(0,1200),webgpu:!!navigator.gpu,resourceCount:performance.getEntriesByType("resource").length})',returnByValue:true})).result;
     for(let sample=0;sample<2;sample++){
      const screenshot=await observer.call('Page.captureScreenshot',{format:'png'});
      await fs.writeFile(path.join(outputDirectory,`${stem}-renderer-${sample}.png`),Buffer.from(screenshot.data,'base64'));
      if(sample===0)await delay(2000);
     }
     }catch(error){record.diagnosticError=String(error);}
    }
    record.observation='host_presented_needs_visual_review';
   }else record.observation='not_presented';
   record.finalState=await client.call('state.get');
   const remote=`/home/feralfile/ff-native-evidence/corpus-${stem}-compositor.png`;
   await execute('ssh',[...sshArgs,sshHost,`XDG_RUNTIME_DIR=/run/user/1000 WAYLAND_DISPLAY=wayland-0 grim ${remote}`]);
   await execute('scp',['-O',...sshArgs,`${sshHost}:${remote}`,path.join(outputDirectory,`${stem}-compositor.png`)]);
   const metrics=await execute('ssh',[...sshArgs,sshHost,'systemctl --user show ff-native-runtime-test -p MemoryCurrent -p MemoryPeak -p CPUUsageNSec -p TasksCurrent']);
   record.cgroup=metrics.stdout;
  }catch(error){record.observation='check_failed';record.error=String(error);}
  record.finishedAt=new Date().toISOString();
  await fs.writeFile(path.join(outputDirectory,'result.json'),JSON.stringify(evidence,null,2)+'\n');
  console.log(`${index+1}/${corpus.playlist.items.length} ${work.title}: ${record.observation}`);
 }
}finally{
 observer?.close();client.close();evidence.finishedAt=new Date().toISOString();
 await fs.writeFile(path.join(outputDirectory,'result.json'),JSON.stringify(evidence,null,2)+'\n');
}
