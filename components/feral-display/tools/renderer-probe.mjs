// Diagnostic renderer evidence only. Never physical-display proof or control.
import {writeFile} from 'node:fs/promises';
import {diagnosticSession} from './diagnostic-cdp.mjs';
const [endpoint, prefix] = process.argv.slice(2);
const base = new URL(endpoint);
if (!['127.0.0.1','localhost','[::1]'].includes(base.hostname)) throw new Error('Use loopback or an SSH tunnel');
const pages = (await (await fetch(new URL('/json', base))).json()).filter(x=>x.type==='page');
if (pages.length !== 1) throw new Error('Expected one artwork target');
const url = new URL(pages[0].webSocketDebuggerUrl); url.host=base.host;
const session = await diagnosticSession(url);
const call = session.call;
try {
  const result=await call('Runtime.evaluate',{expression:`(async()=>({url:location.href,topLevel:top===self,title:document.title,readyState:document.readyState,canvas:[...document.querySelectorAll('canvas')].map(c=>({width:c.width,height:c.height})),webgpu:!!navigator.gpu,adapter:!!(navigator.gpu && await navigator.gpu.requestAdapter()),formats:Object.fromEntries(['video/mp4; codecs="avc1.42E01E"','video/webm; codecs="vp9"','audio/mpeg'].map(t=>[t,document.createElement('video').canPlayType(t)]))}))()`,returnByValue:true,awaitPromise:true});
  await writeFile(prefix+'.json',JSON.stringify(result,null,2)+'\n',{mode:0o600});
  const screenshot=await call('Page.captureScreenshot',{format:'jpeg',quality:90});
  await writeFile(prefix+'.jpg',Buffer.from(screenshot.data,'base64'),{mode:0o600});
  console.log(JSON.stringify(result.result?.value));
} finally {session.close();}
