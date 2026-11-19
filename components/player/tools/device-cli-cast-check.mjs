// Runs the actual enrolled FF-CLI. Admission and host composition have distinct clocks.
import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {connectNative} from './native-client.mjs';
const [socket, cliRoot, device, source, output] = process.argv.slice(2);
assert(output && device && source && path.isAbsolute(cliRoot));
const c = await connectNative(socket, 'cli-cast-observer');
const previous = await c.call('state.get');
const evidence = {startedAt:new Date().toISOString(), source, physicalOutputVerified:false};
const start = performance.now();
let finished = false, exitCode;
const child = spawn('node', ['dist/index.js','display',source,'-d',device], {cwd:cliRoot,stdio:'ignore'});
child.on('error', error=>{evidence.cliError=String(error);finished=true;exitCode=1;});
child.on('exit', code=>{evidence.cliCompletedMs=performance.now()-start;finished=true;exitCode=code;});
try {
  for (let i=0;i<600;i++) {
    const s = await c.call('state.get');
    if(s.selected?.id && s.selected.id!==previous.selected?.id) {
      evidence.selectionMs ??= performance.now()-start;
      if(s.selected.stage==='failed') throw Error(s.selected.error);
      if(s.presented?.id===s.selected.id) {
        evidence.hostCompositionMs ??= performance.now()-start;
        evidence.playlistId=s.playlist?.id; evidence.itemCount=s.playlist?.items?.length;
        evidence.presentedSource=s.presented.item.source;
      }
    }
    if(finished && exitCode!==0) throw Error('FF-CLI exited '+exitCode);
    if(finished && evidence.hostCompositionMs) break;
    await new Promise(r=>setTimeout(r,100));
  }
  assert(finished && evidence.hostCompositionMs,'Cast did not complete and compose in60 seconds');
  evidence.result='passed';
} catch(error) {evidence.result='failed';evidence.error=String(error);throw error;}
finally {c.close();if(!finished)child.kill();await fs.writeFile(output,JSON.stringify(evidence,null,2)+'\n');}
console.log(JSON.stringify(evidence));
