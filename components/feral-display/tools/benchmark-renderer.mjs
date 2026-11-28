// Diagnostic workload benchmark. rAF measures renderer scheduling, not scanout.
import fs from 'node:fs/promises';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {diagnosticSession} from './diagnostic-cdp.mjs';
const run=promisify(execFile);
const [endpoint,host,unit,output]=process.argv.slice(2);
if(!output||!/^ff-native-[a-z-]+\.service$/.test(unit))throw Error('usage: benchmark-renderer.mjs CDP SSH-HOST TEST-UNIT OUTPUT');
const flags=['-o','IPQoS=none','-i',process.env.HOME+'/.ssh/id_ed25519','-o','BatchMode=yes','-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts'];
async function metrics(){const{stdout}=await run('ssh',[...flags,host,'systemctl','--user','show',unit,'-p','CPUUsageNSec','-p','MemoryCurrent','-p','MemoryPeak','-p','TasksCurrent']);return Object.fromEntries(stdout.trim().split('\n').map(x=>{const[k,v]=x.split('=');return[k,Number(v)]}));}
const targets=await(await fetch(endpoint+'/json')).json();const target=targets.find(t=>t.type==='page'&&t.url.includes('2a727b17-7890-4464-913d-3fe5ad2af4ae'));
if(!target)throw Error('Transparent Grit target absent');
const u=new URL(target.webSocketDebuggerUrl);u.host=new URL(endpoint).host;const c=await diagnosticSession(u.href);
const evidence={startedAt:new Date().toISOString(),unit,url:target.url,physicalOutputVerified:false,frameMetric:'requestAnimationFrame scheduling intervals; not GPU presentation or dropped scanout frames',samples:[]};
try{
 evidence.document=(await c.call('Runtime.evaluate',{expression:'({url:location.href,ready:document.readyState,canvas:[...document.querySelectorAll("canvas")].map(c=>[c.width,c.height])})',returnByValue:true})).result.value;
 if(!evidence.document.canvas.length)throw Error('Artwork has no canvas; refusing an idle-page benchmark');
 evidence.before=await metrics();const start=performance.now();
 for(let i=0;i<8;i++){
  const frame=await c.call('Runtime.evaluate',{expression:'new Promise(resolve=>{const a=[];let last=0,start=0;function tick(t){if(!start)start=t;if(last)a.push(t-last);last=t;if(t-start<15000)requestAnimationFrame(tick);else resolve(a)}requestAnimationFrame(tick)})',returnByValue:true,awaitPromise:true});
  evidence.samples.push({intervals:frame.result.value,metrics:await metrics()});
 }
 evidence.elapsedSeconds=(performance.now()-start)/1000;evidence.after=await metrics();
 evidence.cpuPercentOneCore=(evidence.after.CPUUsageNSec-evidence.before.CPUUsageNSec)/1e9/evidence.elapsedSeconds*100;
 const a=evidence.samples.flatMap(x=>x.intervals).sort((x,y)=>x-y);
 evidence.frameIntervals={count:a.length,medianMs:a[Math.floor(a.length*.5)],p95Ms:a[Math.floor(a.length*.95)],p99Ms:a[Math.floor(a.length*.99)],over33ms:a.filter(x=>x>33).length};
 const shot=await c.call('Page.captureScreenshot',{format:'jpeg',quality:80});await fs.writeFile(output+'.jpg',Buffer.from(shot.data,'base64'));
 evidence.result='measured';
}catch(e){evidence.result='failed';evidence.error=String(e);process.exitCode=1;}finally{c.close();await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');}
