// Diagnostics and one-shot legacy export only. Not a player control interface.
export async function diagnosticSession(address) {
  const ws = new WebSocket(address);
  const pending = new Map();
  const observers = new Map();
  let sequence = 0;
  const fail = error => {
    for (const request of pending.values()) { clearTimeout(request.timer); request.reject(error); }
    pending.clear();
  };
  ws.addEventListener('close', () => fail(new Error('Diagnostic CDP connection closed')));
  ws.addEventListener('error', () => fail(new Error('Diagnostic CDP connection failed')));
  ws.addEventListener('message', ({data}) => {
    let reply;
    try { reply = JSON.parse(data); } catch { fail(new Error('Invalid diagnostic response')); ws.close(); return; }
    if(reply.method){for(const fn of observers.get(reply.method)||[])fn(reply.params);return;}
    const request = pending.get(reply.id);
    if (!request) return;
    pending.delete(reply.id); clearTimeout(request.timer);
    if (reply.error) request.reject(new Error(JSON.stringify(reply.error)));
    else request.resolve(reply.result);
  });
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => { ws.close(); reject(new Error('Diagnostic connection timed out')); }, 10000);
    ws.addEventListener('open', () => { clearTimeout(timer); resolve(); }, {once:true});
    ws.addEventListener('error', () => { clearTimeout(timer); reject(new Error('Diagnostic connection failed')); }, {once:true});
    ws.addEventListener('close', () => { clearTimeout(timer); reject(new Error('Diagnostic connection closed')); }, {once:true});
  });
  return {
    observe(method,fn){
      const listeners=observers.get(method)||new Set();listeners.add(fn);observers.set(method,listeners);
      return ()=>listeners.delete(fn);
    },
    call(method, params) {
      if (ws.readyState !== WebSocket.OPEN) return Promise.reject(new Error('Diagnostic connection is not open'));
      const id = ++sequence;
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => { pending.delete(id); reject(new Error(`Diagnostic timeout: ${method}`)); }, 20000);
        pending.set(id, {resolve, reject, timer});
        try { ws.send(JSON.stringify({id, method, params})); }
        catch (error) { clearTimeout(timer); pending.delete(id); reject(error); }
      });
    },
    close() { fail(new Error('Diagnostic session ended')); ws.close(); },
  };
}
