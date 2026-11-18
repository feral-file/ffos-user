// Isolated, fixed-seed diagnostic. This does not control production playback.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {createHash} from 'node:crypto';
import {diagnosticSession} from './diagnostic-cdp.mjs';
const run = promisify(execFile);
const [endpoint, host, unit, manifestPath, output, windows = '6', configurationPath] = process.argv.slice(2);
assert(output && /^ff-native-[a-z-]+\.service$/.test(unit), 'usage: benchmark-matched.mjs CDP SSH-HOST TEST-UNIT FIXTURE-MANIFEST OUTPUT [10s-WINDOWS] [CONFIG-JSON]');
assert(Number.isInteger(+windows) && +windows >= 1 && +windows <= 60);
const expected = JSON.parse(await fs.readFile(manifestPath));
const configuration = configurationPath ? JSON.parse(await fs.readFile(configurationPath)) : {
 chromiumVersion:'156.0.8078.80', viewport:[1920,1920,1],
 renderer:'ANGLE (AMD, AMD Radeon Graphics (radeonsi rembrandt ACO), OpenGL ES 3.2)'
};
assert(/^\d+\.\d+\.\d+\.\d+$/.test(configuration.chromiumVersion));
assert(configuration.viewport?.length===3&&configuration.viewport.every(v=>Number.isFinite(v)&&v>0));
assert(typeof configuration.renderer==='string'&&configuration.renderer.length>0);
const flags = ['-o','IPQoS=none','-i',process.env.HOME+'/.ssh/id_ed25519','-o','BatchMode=yes','-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts'];
async function metrics() {
 const {stdout} = await run('ssh', [...flags, host, 'systemctl','--user','show',unit,'-p','CPUUsageNSec','-p','MemoryCurrent','-p','MemoryPeak','-p','TasksCurrent']);
 const values = Object.fromEntries(stdout.trim().split('\n').map(x => {const [k,v]=x.split('='); return [k,Number(v)];}));
 assert(values.CPUUsageNSec > 0 && values.MemoryCurrent > 0, 'Test unit is not running');
 return values;
}
const version = await (await fetch(endpoint+'/json/version')).json();
assert(version.Browser.endsWith('/'+configuration.chromiumVersion), `Engine mismatch: ${version.Browser}`);
const targets = await (await fetch(endpoint+'/json')).json();
const target = targets.find(t => t.type === 'page' && t.url === 'http://127.0.0.1:18990/grit-fixed.html');
assert(target, 'Fixed-seed fixture target absent');
const u = new URL(target.webSocketDebuggerUrl); u.host = new URL(endpoint).host;
const c = await diagnosticSession(u.href);
const browserUrl = new URL(version.webSocketDebuggerUrl); browserUrl.host = new URL(endpoint).host;
const browser = await diagnosticSession(browserUrl.href);
let tracing = false;
const traceEvents = [];
browser.observe('Tracing.dataCollected', p => traceEvents.push(...p.value.filter(e => e.name === 'Display::FrameDisplayed')));
const traceDone = new Promise(resolve => browser.observe('Tracing.tracingComplete', resolve));
async function remoteClock() {
 const {stdout}=await run('ssh',[...flags,host,`python3 -c 'import time,json,glob;print(json.dumps({"monotonicNs":time.monotonic_ns(),"realtimeNs":time.time_ns(),"deviceGpuBytes":{p:int(open(p).read()) for p in glob.glob("/sys/class/drm/card*/device/mem_info_*_used")}}))'`]);
 return JSON.parse(stdout);
}
const evidence = {startedAt:new Date().toISOString(), unit, version, configuration, fixture:expected,
 physicalOutputVerified:false, frameMetric:'rAF timestamp intervals and callback delivery intervals (performance.now); output measured separately', samples:[]};
try {
 const expression = `(()=>{const c=document.querySelector('canvas'),g=c?.getContext('webgl'),e=g?.getExtension('WEBGL_debug_renderer_info');return {fixture:window.__ffBenchmark,ready:document.readyState,visible:document.visibilityState,viewport:[innerWidth,innerHeight,devicePixelRatio],canvas:c&&[c.width,c.height],renderer:e&&g.getParameter(e.UNMASKED_RENDERER_WEBGL),glVersion:g?.getParameter(g.VERSION)}})()`;
 evidence.document = (await c.call('Runtime.evaluate', {expression,returnByValue:true})).result.value;
 assert.deepEqual(evidence.document.fixture, {seed:expected.seed,sourceSha256:expected.sourceSha256,sourceUrl:expected.sourceUrl});
 assert.deepEqual(evidence.document.viewport, configuration.viewport);
 assert.deepEqual(evidence.document.canvas, configuration.viewport.slice(0,2).map(v=>v*configuration.viewport[2]));
 assert.equal(evidence.document.visible, 'visible');
 assert.equal(evidence.document.renderer, configuration.renderer);
 const body = (await c.call('Runtime.evaluate', {expression:'fetch(location.href).then(r=>r.text())',awaitPromise:true,returnByValue:true})).result.value;
 assert.equal(createHash('sha256').update(body).digest('hex'), expected.fixtureSha256);
 // Warm up shaders, allocations and the entire ten-second animation cycle.
 await new Promise(r=>setTimeout(r,20000));
 await browser.call('Tracing.start', {categories:'-*,disabled-by-default-display.framedisplayed',options:'record-until-full'}); tracing=true;
 evidence.before = await metrics(); evidence.clockStart=await remoteClock(); const start = performance.now();
 for (let i=0;i<+windows;i++) {
  const r = await c.call('Runtime.evaluate', {expression:`new Promise(resolve=>{const a=[],delivery=[];let last=0,start=0,lastDelivery=0;function tick(t){const now=performance.now();if(!start)start=t;if(last){a.push(t-last);delivery.push(now-lastDelivery);}last=t;lastDelivery=now;if(t-start<10000)requestAnimationFrame(tick);else resolve({intervals:a,delivery})}requestAnimationFrame(tick)})`,returnByValue:true,awaitPromise:true});
  assert(Array.isArray(r.result.value?.intervals) && r.result.value.intervals.length > 10, 'No animation samples');
  evidence.samples.push({...r.result.value,metrics:await metrics()});
 }
 evidence.elapsedSeconds=(performance.now()-start)/1000; evidence.clockEnd=await remoteClock(); evidence.after=await metrics();
 await browser.call('Tracing.end'); await traceDone; tracing=false;
 assert(traceEvents.length > 10, 'No compositor feedback trace');
 evidence.compositorTrace={meaning:unit.includes('runtime')?'CEF offscreen compositor feedback; use native Wayland telemetry for output':'Native browser display feedback', events:traceEvents};
 evidence.cpuPercentOneCore=(evidence.after.CPUUsageNSec-evidence.before.CPUUsageNSec)/1e9/evidence.elapsedSeconds*100;
 const a=evidence.samples.flatMap(x=>x.intervals).sort((a,b)=>a-b);
 const mean=a.reduce((a,b)=>a+b,0)/a.length;
 evidence.frameIntervals={count:a.length,meanMs:mean,medianMs:a[Math.floor(a.length*.5)],p95Ms:a[Math.floor(a.length*.95)],p99Ms:a[Math.floor(a.length*.99)],standardDeviationMs:Math.sqrt(a.reduce((s,x)=>s+(x-mean)**2,0)/a.length),over50ms:a.filter(x=>x>50.5).length};
 const delivery=evidence.samples.flatMap(x=>x.delivery).sort((a,b)=>a-b);
 evidence.callbackDelivery={count:delivery.length,meanMs:delivery.reduce((a,b)=>a+b,0)/delivery.length,medianMs:delivery[Math.floor(delivery.length*.5)],p95Ms:delivery[Math.floor(delivery.length*.95)],p99Ms:delivery[Math.floor(delivery.length*.99)]};
 const shot=await c.call('Page.captureScreenshot',{format:'jpeg',quality:80}); await fs.writeFile(output+'.jpg',Buffer.from(shot.data,'base64'));
 evidence.result='measured';
} catch(error) { evidence.result='failed'; evidence.error=String(error); process.exitCode=1; }
finally {if(tracing) {try {await browser.call('Tracing.end');} catch {}} browser.close();c.close();await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n'); console.log(JSON.stringify({result:evidence.result,frames:evidence.frameIntervals,cpu:evidence.cpuPercentOneCore,error:evidence.error}));}
