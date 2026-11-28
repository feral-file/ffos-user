import json, os, pathlib, socket, subprocess, sys, time
from pathlib import Path
root = Path.home() / 'ff-direct20'
name = sys.argv[1]
direct = name.startswith('direct')
source = 'http://127.0.0.1:18994/clip.mp4'
display = {'loop':True, 'scaling':'fit','background':'#000000','margin':0}
props = {}
cgroup = Path('/sys/fs/cgroup') / subprocess.check_output(['systemctl','--user','show','-p','ControlGroup','--value','chromium-kiosk.service'], text=True).strip().lstrip('/')
def sample():
    cpu = dict(line.split() for line in (cgroup / 'cpu.stat').read_text().splitlines())
    pid = subprocess.check_output(['pgrep','-n','-f','^/app/ff-media-worker$'],text=True).strip()
    clients = {}
    for path in Path('/proc',pid,'fdinfo').glob('*'):
        try: info = dict(line.split(':',1) for line in path.read_text().splitlines() if ':' in line)
        except FileNotFoundError: continue
        if 'drm-engine-dec' in info: clients[info['drm-client-id']] = int(info['drm-engine-dec'].split()[0])
    if not clients: raise RuntimeError('No decode engine counter')
    return dict(drmDecodeNs=sum(clients.values()), ns=time.monotonic_ns(), cpuUsec=int(cpu['usage_usec']), mem=int((cgroup/'memory.current').read_text()), props=props.copy())
def capture(number):
    subprocess.run(['grim','-s','0.25',str(root / f'{name}-{number}.png')],env=dict(os.environ,WAYLAND_DISPLAY='wayland-0'),check=True)
if direct:
    parent, child = socket.socketpair(socket.AF_UNIX,socket.SOCK_SEQPACKET)
    launch = str(root/'stage/media/ff-media-launch')
    out = open(root/f'{name}.timing.json','w')
    actions = [(os.POSIX_SPAWN_CLOSE,parent.fileno()),(os.POSIX_SPAWN_DUP2,child.fileno(),3),(os.POSIX_SPAWN_DUP2,out.fileno(),1)]
    pid = os.posix_spawn(launch,[launch],os.environ,file_actions=actions)
    child.close()
    parent.settimeout(.2)
    def send(value): parent.send(json.dumps(value).encode())
    send(dict(method='initialize',source=source,width=1920,height=1920,paused=False,muted=False,volume=1,display=display,diagnosticDirectOutput=True))
    def update():
        try: data=parent.recv(65536)
        except TimeoutError: return
        if not data: raise RuntimeError('worker disconnected')
        msg=json.loads(data)
        if msg['event']=='error': raise RuntimeError(msg)
        if msg['event']=='property': props[msg['name']]=msg['value']
else:
    sys.path.insert(0,str(Path.home()/'ff-public-source-20261118/tools'))
    from playerctl import exchange
    import uuid
    sock = socket.socket(socket.AF_UNIX)
    sock.settimeout(4)
    sock.connect('/run/user/1000/ff-player/player.sock')
    def call(method,params):
        hello=exchange(sock,dict(v=1,id='hello',method='hello',params=dict(client='bench',versions=[1])))
        return exchange(sock,dict(v=1,id='req',method=method,params=params,instance=hello['instance'],generation=hello['snapshot']['generation'],deadlineMs=int(time.time()*1000)+3500,key=str(uuid.uuid4())))
    call('playback.replace',dict(playlist=dict(items=[dict(id=name,title=name,source=source,duration=0,display=display)])))
    def update():
        global props
        state=call('state.get',{})
        if state['renderer']=='failed': raise RuntimeError(state['selected'])
        props=state.get('media') or {}
        time.sleep(.15)
limit=time.monotonic()+40
while props.get('hwdec-current')!='vaapi' or (props.get('time-pos') or 0)<7:
    update()
    if time.monotonic()>limit: raise RuntimeError(props)
if props.get('current-ao')!='pulse': raise RuntimeError(f'PulseAudio inactive: {props}')
capture(1)
while (props.get('time-pos') or 0)<10: update()
capture(2)
samples=[sample()]
for k in range(1,7):
    while time.monotonic_ns()<samples[0]['ns']+k*10_000_000_000: update()
    samples.append(sample())
if any(s['props'].get('hwdec-current')!='vaapi' for s in samples): raise RuntimeError('VA-API lost')
(root/f'{name}.json').write_text(json.dumps(samples,indent=2))
if direct:
    send({'method':'stop'})
    os.waitpid(pid,0)
