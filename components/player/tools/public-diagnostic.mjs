// Explicit diagnostic opt-in only. Normal playback never opens a debugging port.
import {execFile,spawn} from 'node:child_process';
import {promisify} from 'node:util';
import net from 'node:net';
import assert from 'node:assert/strict';
export async function publicDiagnostic(occurrence,launcherPid) {
 const host=process.env.FF_ENGINE_SSH,root=process.env.FF_ENGINE_PROFILE_ROOT;
 assert(/^[a-zA-Z0-9@.:-]+$/.test(host||'')&&/^\/[a-zA-Z0-9/_.-]+$/.test(root||'')&&/^[a-f0-9-]+$/.test(occurrence),'Explicit trusted SSH/profile/occurrence required');
 const args=['-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts','-o','BatchMode=yes','-i',process.env.HOME+'/.ssh/id_ed25519'];
 const {stdout}=await promisify(execFile)('ssh',[...args,host,'cat',`${root}/${occurrence}/DevToolsActivePort`]);
 let remote=Number(stdout.split('\n')[0]);assert(remote>0&&remote<65536);let proxy;
 if(process.env.FF_ENGINE_NETNS==='1'){
  assert(Number.isInteger(launcherPid)&&launcherPid>1);
  proxy=spawn('ssh',[...args,host,'sudo','-n','python3','/tmp/ff-diagnostic-netns-proxy.py',String(launcherPid),String(remote)],{stdio:['pipe','pipe','inherit']});
  remote=await new Promise((resolve,reject)=>{let data='';const timer=setTimeout(()=>{proxy.kill();reject(Error('Namespace proxy timeout'));},5000);proxy.stdout.on('data',part=>{data+=part;if(data.includes('\n')){clearTimeout(timer);resolve(Number(data.split('\n')[0]));}});proxy.on('exit',()=>reject(Error('Namespace proxy closed')));});assert(remote>0&&remote<65536);
 }
 const server=net.createServer();await new Promise(r=>server.listen(0,'127.0.0.1',r));const port=server.address().port;await new Promise(r=>server.close(r));
 const tunnel=spawn('ssh',[...args,'-N','-o','ExitOnForwardFailure=yes','-L',`${port}:127.0.0.1:${remote}`,host],{stdio:'ignore'});
 tunnel.on('error',()=>{});
 for(let i=0;i<50;i++){try{await fetch(`http://127.0.0.1:${port}/json/version`);return {endpoint:`http://127.0.0.1:${port}`,close:()=>{tunnel.kill();proxy?.stdin.end();}};}catch{}await new Promise(r=>setTimeout(r,100));}
 tunnel.kill();proxy?.stdin.end();throw Error('Diagnostic tunnel failed');
}
