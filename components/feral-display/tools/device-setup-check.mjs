// Compositor evidence, separate from renderer screenshots and physical output.
// Uses synthetic setup credentials only; never capture a real claim token here.
import {connectNative} from './native-client.mjs';
import {execFileSync} from 'node:child_process';
import {mkdirSync,writeFileSync} from 'node:fs';
import {resolve} from 'node:path';
import assert from 'node:assert/strict';
const [socket,host,outArg]=process.argv.slice(2),out=resolve(outArg);
mkdirSync(out,{recursive:true});
const native=await connectNative(socket,'setup-check');
const flags=['-o','IPQoS=none','-i',process.env.HOME+'/.ssh/id_ed25519','-o','BatchMode=yes','-o','UserKnownHostsFile=/tmp/ff-player-native-known-hosts'];
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const evidence={startedAt:new Date().toISOString(),capture:'Cage compositor, not physical panel',checks:[]};
const cases=[
 ['softap_qr',{ssid:'FF1-test:;','password':'fake;password','portal_url':'http://10.42.0.1/'}],
 ['softap_qr',{ssid:'FF1-test:;','password':'fake;password','portal_url':'http://10.42.0.1/',client_attached:true}],
 ['claim_qr',{url:'https://example.org/claim?token=SYNTHETIC-TEST',device_name:'My Art Computer'}],
 ['scanning',{}],['joining',{}],['finalizing',{}],
 ['connecting',{reason:'Checking the network connection…'}],
 ['join_failed',{reason:'The password was rejected. Please try again.'}],
 ['setup_error',{reason:'The setup hotspot could not start. Automatic retries continue.'}],
 ['updating',{progress:42}],['factory_reset',{}],
];
try {
 await native.call('playback.stop');
 for(let i=0;i<cases.length;i++){
  const [state,fields]=cases[i];
  await native.call('overlay.set',{owner:'setup',state,fields});
  const snapshot=await native.call('state.get');
  assert.equal(snapshot.overlay.state,state);assert.equal(snapshot.host,'responsive');
  await delay(300);
  const name=String(i).padStart(2,'0')+'-'+state+'.png';
  const remote='/home/feralfile/ff-native-evidence/setup-'+name;
  execFileSync('ssh',[...flags,host,'/usr/bin/env XDG_RUNTIME_DIR=/run/user/1000 WAYLAND_DISPLAY=wayland-0 /usr/bin/grim '+remote],{timeout:10000});
  execFileSync('scp',['-O',...flags,host+':'+remote,resolve(out,name)],{timeout:10000});
  evidence.checks.push({state,capture:name,host:snapshot.host});
 }
 await native.call('overlay.set',{owner:'setup',state:'ready'});
 assert.equal((await native.call('state.get')).overlay,null);
 evidence.checks.push({state:'ready',cleared:true});evidence.result='passed';
} catch(error){evidence.result='failed';evidence.error=String(error);throw error;}
finally{native.close();evidence.finishedAt=new Date().toISOString();writeFileSync(resolve(out,'result.json'),JSON.stringify(evidence,null,2)+'\n');}
