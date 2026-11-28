#!/usr/bin/env python3
# Synthetic original-origin resource fixture; import only while controld is stopped.
from pathlib import Path
import hashlib,json,sys
project=Path(sys.argv[1]);root=Path(sys.argv[2]);(root/'bodies').mkdir(parents=True,exist_ok=True)
base='http://127.0.0.1:18990';source=base+'/offline-contract.html'
html='''<!doctype html><style>@font-face{font-family:trial;src:url('/offline-font.woff')}body{font-family:trial;background:white;color:black}</style><h1>Offline contract</h1><pre id="result"></pre><iframe src="/offline-frame.html"></iframe><script>
window.results={};function show(k,v){results[k]=v;document.querySelector('#result').textContent=JSON.stringify(results,null,2)}
document.fonts.load('20px trial').then(x=>show('font',x.length>0));
WebAssembly.instantiateStreaming(fetch('/offline.wasm')).then(x=>show('wasm',x.instance.exports.ans())).catch(e=>show('wasm',String(e)));
fetch('/offline-redirect').then(r=>r.text()).then(t=>show('redirect',t));
setTimeout(()=>fetch('/offline-data').then(r=>r.text()).then(t=>show('runtimeFetch',t)),600);
window.addEventListener('message',e=>show(e.data.key,e.data.value));
let worker=new Worker('/offline-worker.js');worker.onmessage=e=>show('worker',e.data);
navigator.serviceWorker.register('/offline-sw.js').then(()=>show('serviceWorker','registered')).catch(e=>show('serviceWorker','unavailable'));
</script>'''
resources={'/offline-contract.html':(html.encode(),'text/html'),'/offline-frame.html':(b'<script>parent.postMessage({key:"frame",value:"same-origin"},location.origin)</script>','text/html'),'/offline-data':(b'original-response','text/plain'),'/offline-worker.js':(b'fetch("/offline-worker-data").then(r=>r.text()).then(t=>postMessage(t)).catch(e=>postMessage("runtime-dependency-unavailable"))','text/javascript'),'/offline-worker-data':(b'worker-runtime-success','text/plain'),'/offline-sw.js':(b'self.addEventListener("install",e=>self.skipWaiting());','text/javascript'),'/offline.wasm':(bytes.fromhex('0061736d010000000105016000017f0302010007070103616e7300000a06010400412a0b'),'application/wasm'),'/offline-font.woff':((project/'assets/fonts/PPMori-Regular.woff').read_bytes(),'font/woff'),'/offline-redirect':(b'','text/plain')}
index={}
for path,(body,mime)in resources.items():
 sha=hashlib.sha256(body).hexdigest();(root/'bodies'/sha).write_bytes(body)
 fields=[{'name':'Content-Type','value':mime},{'name':'Content-Security-Policy','value':"default-src 'self';script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval';style-src 'self' 'unsafe-inline';worker-src 'self';frame-src 'self'"},{'name':'X-Frame-Options','value':'SAMEORIGIN'}]
 if path=='/offline-redirect':fields.append({'name':'Location','value':base+'/offline-data'})
 index['GET '+base+path]={'status':302 if path=='/offline-redirect'else 200,'headers':fields,'sha256':sha,'size':len(body),'requestHeaders':{}}
(root/'responses.json').write_text(json.dumps(index,indent=2));print(source)
