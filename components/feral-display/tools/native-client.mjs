// Diagnostic/test client only; production controld uses the Go playerlink client.
import net from 'node:net';
import {randomUUID} from 'node:crypto';
import assert from 'node:assert/strict';

export async function connectNative(socketPath, clientName = "native-diagnostic") {
  const socket=net.createConnection(socketPath);
  await new Promise((resolve,reject)=>{socket.once('connect',resolve);socket.once('error',reject);});
  let buffer=Buffer.alloc(0), waiting=null;
  const fail=error=>{if(waiting){clearTimeout(waiting.timer);waiting.reject(error);waiting=null;}};
  socket.on('error',fail);socket.on('close',()=>fail(new Error('native connection closed')));
  socket.on('data',data=>{
    buffer=Buffer.concat([buffer,data]);
    while(buffer.length>=4){
      const length=buffer.readUInt32BE();
      if(length<1||length>4*1024*1024){fail(new Error('bad frame length'));socket.destroy();return;}
      if(buffer.length<length+4)return;
      const body=JSON.parse(buffer.subarray(4,4+length));buffer=buffer.subarray(4+length);
      if(!waiting)continue;
      const request=waiting;waiting=null;clearTimeout(request.timer);
      if(body.id!==request.id)request.reject(new Error('correlation mismatch'));
      else if(body.error)request.reject(Object.assign(new Error(body.error.message),{code:body.error.code}));
      else request.resolve(body.result);
    }
  });
  async function exchange(request){
    assert.equal(waiting,null);
    return new Promise((resolve,reject)=>{
      waiting={resolve,reject,id:request.id,timer:setTimeout(()=>{fail(new Error('native request timeout'));socket.destroy();},5000)};
      const body=Buffer.from(JSON.stringify(request)),prefix=Buffer.alloc(4);prefix.writeUInt32BE(body.length);
      socket.write(Buffer.concat([prefix,body]));
    });
  }
  return {close:()=>socket.destroy(),async call(method,params={}){
    const hello=await exchange({v:1,id:randomUUID(),method:'hello',params:{client:clientName,versions:[1]}});
    return exchange({v:1,id:randomUUID(),method,params,instance:hello.instance,generation:hello.snapshot.generation,key:randomUUID(),deadlineMs:Date.now()+4000});
  }};
}
