// The public-engine bundle fetches only its own bounded configuration. Artist
// documents never load this file or receive a device-control binding.
const publicBundle = location.hostname === 'ff-player.internal';
if (publicBundle) {
 window.ffRenderer = (kind, detail='') => {
  if (kind === 'configure') return;
  const event = kind === 'configured' ? `configured/${detail}` : kind;
  fetch(`./events/${event}`, {cache:'no-store'}).catch(()=>{});
 };
}
// Bundled image/GLB renderer. Direct video/audio is exclusively native libmpv.
// This document has no playback, label, persistence or device authority.
let current, element, latest;
const emit = (kind, detail = '') => window.ffRenderer(kind, detail);
window.addEventListener('unhandledrejection', event => emit('error', event.reason?.message || 'Bundled renderer operation failed'));
window.addEventListener('error', event => { if(event.message) emit('error',event.message); });
const applyDisplay = () => {
  if (element) element.style.objectFit = ({fit:'contain',fill:'cover',stretch:'fill',auto:'contain'})[latest.display.scaling] || 'contain';
  if (element) {
    document.body.style.background = latest.display.background || '#000000';
    const revision = latest.displayRevision;
    // A focused renderer acknowledges its own layout only. The host still
    // requires a subsequent captured frame and native submission before status.
    requestAnimationFrame(() => requestAnimationFrame(() => {
      if (latest.displayRevision === revision) emit('configured', String(revision));
    }));
  }
};
window.ffConfigure = async raw => {
  try {
    const config = JSON.parse(raw);
    latest = config;
    if(element?.tagName==='MODEL-VIEWER'){if(config.paused)element.pause();else element.play();}
    if (current === config.source) { applyDisplay(); return; }
    if (current) throw new Error('Renderer source cannot change within an occurrence');
    current = config.source;
    const ext = new URL(current).pathname.toLowerCase().split('.').pop();
    const model = config.mime.startsWith('model/') || ['glb', 'gltf'].includes(ext);
    if (model) {
      // 4.3.1's element constructor reapplies this global configuration. A
      // static setter before construction alone is overwritten by CDN defaults.
      window.ModelViewerElement = {
        dracoDecoderLocation: new URL('./draco/', import.meta.url).href,
        ktx2TranscoderLocation: new URL('./basis/', import.meta.url).href,
      };
      await import('./model-viewer.min.js');
    }
    element?.remove(); element = document.createElement(model ? 'model-viewer' : 'img'); element.id = 'art';
    // Settings can arrive while the viewer module is still importing. Apply
    // the latest setting after construction without creating a second viewer.
    applyDisplay();
    element.addEventListener('error', event => emit('error', event.detail?.sourceError?.message || 'Artwork decoding or resource loading failed'));
    if (model) {
      // Match ff-player ModelViewerScreen, including its animation behavior.
      for (const [key,value] of Object.entries({autoplay:'','camera-controls':'',crossorigin:'anonymous',exposure:'1',loading:'eager',reveal:'auto','shadow-intensity':'1'})) element.setAttribute(key,value);
      element.addEventListener('load', () => {if(latest.paused)element.pause();emit('ready');});
    } else element.addEventListener('load', async () => {
      try { await element.decode(); emit('ready'); }
      catch (error) { emit('error', `Image decode failed: ${error.message}`); }
    });
    document.body.append(element); element.src = current;
  } catch (error) { emit('error', error.message); }
};
emit('configure');

if (publicBundle) {
 const configure = async () => {
  const response=await fetch('./config.json',{cache:'no-store'});
  if(!response.ok)throw new Error('Bundled renderer configuration unavailable');
  const next=await response.text();if(next!==window.lastNativeConfig){window.lastNativeConfig=next;await window.ffConfigure(next);}
 };
 configure().catch(error=>emit('error',error.message));
 setInterval(()=>configure().catch(error=>emit('error',error.message)),250);
}
