#!/usr/bin/env python3
"""Bounded public-Chromium trial. Diagnostic CDP is private inherited pipes.

This tool does not replace the installed product engine/compositor. Its evidence
must never be described as the native host supporting capsule-selected engines.
Capture preserves the observed response and rejects unsupported session variants.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import select
import re
import subprocess
import time


class Pipe:
    def __init__(self, executable, flags, log_path, prefix=()):
        # Reserve the conventional descriptors before creating pipe endpoints.
        holders = [os.open('/dev/null', os.O_RDONLY) for _ in range(2)]
        command_read, command_write = os.pipe()
        reply_read, reply_write = os.pipe()
        for fd in holders:
            os.close(fd)
        os.dup2(command_read, 3)
        os.dup2(reply_write, 4)
        os.close(command_read)
        os.close(reply_write)
        self.log = log_path.open('wb')
        self.process = subprocess.Popen([*prefix, executable, *flags, '--remote-debugging-pipe', 'about:blank'],
                                        pass_fds=(3, 4), stdout=subprocess.DEVNULL, stderr=self.log)
        os.close(3)
        os.close(4)
        self.writer = os.fdopen(command_write, 'wb', buffering=0)
        self.reader = os.fdopen(reply_read, 'rb', buffering=0)
        self.buffer = b''
        self.sequence = 0
        self.responses = {}
        self.events = []
        self.handler = None
        self.delivering = False
        self.deadline = time.monotonic() + 180

    def pump(self):
        while b'\0' not in self.buffer:
            remaining = self.deadline - time.monotonic()
            if remaining <= 0 or not select.select([self.reader], [], [], remaining)[0]:
                raise TimeoutError('engine wall-clock deadline')
            block = os.read(self.reader.fileno(), 65536)
            if not block:
                raise RuntimeError('engine pipe closed')
            self.buffer += block
            if len(self.buffer) > 128 * 1024 * 1024:
                raise RuntimeError('engine response exceeds trial limit')
        data, _, self.buffer = self.buffer.partition(b'\0')
        message = json.loads(data)
        if 'id' in message:
            self.responses[message['id']] = message
        else:
            self.events.append(message)
        if self.handler and not self.delivering:
            self.delivering = True
            try:
                while self.events:
                    self.handler(self.events.pop(0))
            finally:
                self.delivering = False

    def call(self, method, params=None, session=None):
        self.sequence += 1
        identity = self.sequence
        request = dict(id=identity, method=method, params=params or {})
        if session:
            request['sessionId'] = session
        self.writer.write(json.dumps(request).encode() + b'\0')
        while identity not in self.responses:
            self.pump()
        response = self.responses.pop(identity)
        if 'error' in response:
            raise RuntimeError(f'{method}: {response["error"]}')
        return response.get('result', {})

    def close(self):
        self.deadline=min(self.deadline,time.monotonic()+5)
        try:
            self.call('Browser.close')
        except (OSError, RuntimeError, TimeoutError):
            self.process.kill()
        try:
            self.process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)
        self.reader.close()
        self.writer.close()
        self.log.close()


def process_evidence(executable):
    observations=[]
    for entry in Path('/proc').glob('[0-9]*/cmdline'):
        try:
            words=entry.read_bytes().split(b'\0')
            command=b' '.join(words).decode()
            if not command.startswith(executable+' ') or '--type=' in command:continue
            pid=entry.parent.name
            status=dict(line.split(':',1) for line in (entry.parent/'status').read_text().splitlines() if ':' in line)
            observations.append(dict(pid=int(pid),networkNamespace=os.readlink(entry.parent/'ns/net'),
                observerNetworkNamespace=os.readlink('/proc/self/ns/net'),interfaces=(entry.parent/'net/dev').read_text(),
                capabilities={k:status.get(k,'').strip() for k in ('Uid','CapEff','CapBnd','NoNewPrivs')}))
        except (OSError,UnicodeError):continue
    return observations


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--engine', required=True)
    parser.add_argument('--url', required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--capture', type=Path)
    parser.add_argument('--replay', type=Path)
    parser.add_argument('--headed', action='store_true')
    parser.add_argument('--isolate-network', action='store_true')
    parser.add_argument('--seconds', type=int, default=30)
    args = parser.parse_args()
    if args.capture and args.replay:
        parser.error('capture and replay are exclusive')
    args.output.mkdir(parents=True, exist_ok=False)
    profile = args.output / 'profile'
    flags = ['--enable-gpu', '--no-first-run', '--no-default-browser-check',
             '--disable-background-networking', '--disable-extensions', '--disable-dev-shm-usage',
             '--enable-unsafe-webgpu', '--use-angle=vulkan', '--enable-features=Vulkan,VulkanFromANGLE',
             '--disable-vulkan-surface', '--window-size=1920,1080', f'--user-data-dir={profile}']
    flags += ['--ozone-platform=wayland', '--kiosk'] if args.headed else ['--headless=new']
    # The executable is operator supplied; an artwork/capture never supplies flags.
    log = args.output / 'engine.log'
    prefix = []
    if args.isolate_network:
        prefix = ['bwrap', '--unshare-user', '--unshare-net', '--unshare-pid',
                  '--die-with-parent', '--ro-bind', '/', '/', '--dev-bind', '/dev', '/dev',
                  '--proc', '/proc', '--tmpfs', '/tmp', '--bind', str(args.output.resolve()),
                  str(args.output.resolve())]
    evidence = dict(url=args.url, mode='replay' if args.replay else 'live',
                    runnerSha256=hashlib.file_digest(open(__file__,'rb'),'sha256').hexdigest(),freshProfile=True, headed=args.headed, kernelNetworkIsolated=args.isolate_network, errors=[], failures=[], targets=[], responses=[], misses=[])
    table = {}
    requests = {}
    if args.replay:
        index=args.replay/'responses.json'
        if index.stat().st_size>16*1024*1024: raise ValueError('replay index exceeds bound')
        table = json.loads(index.read_text())
        if not isinstance(table,dict) or len(table)>4096: raise ValueError('invalid replay index')
        for response in table.values():
            if not re.fullmatch('[0-9a-f]{64}',response['sha256']): raise ValueError('invalid body digest')
            if not 0<=response['size']<=32*1024*1024: raise ValueError('replay resource exceeds bound')
            path=args.replay/'bodies'/response['sha256']
            if path.parent.is_symlink() or path.is_symlink() or path.stat().st_size!=response['size']: raise ValueError('invalid body file')
            if not 100<=response['status']<=599: raise ValueError('invalid HTTP status')
    if args.capture:
        args.capture.mkdir(parents=True, exist_ok=False)
        (args.capture / 'bodies').mkdir()
    pending = {}
    sessions = {}
    def handle(event):
        method = event.get('method')
        data = event.get('params', {})
        session = event.get('sessionId')
        if method == 'Target.attachedToTarget':
            attached = data['sessionId']
            info = data['targetInfo']
            evidence['targets'].append(info)
            sessions[info['targetId']] = attached
            if info['type'] in ('page', 'iframe', 'worker', 'service_worker', 'shared_worker'):
                pipe.call('Runtime.enable', session=attached)
                pipe.call('Network.enable', dict(maxTotalBufferSize=64*1024*1024,
                                                maxResourceBufferSize=32*1024*1024), attached)
                pipe.call('Network.setCacheDisabled', dict(cacheDisabled=True), attached)
                pipe.call('Target.setAutoAttach', dict(autoAttach=True, waitForDebuggerOnStart=True,
                                                      flatten=True), attached)
                if (args.replay or args.capture) and info['type'] in ('page','iframe'):
                    pipe.call('Fetch.enable', dict(patterns=[dict(urlPattern='*',requestStage='Request' if args.replay else 'Response')]), attached)
                pipe.call('Runtime.runIfWaitingForDebugger', session=attached)
        elif method == 'Network.requestWillBeSent':
            if args.capture and 'redirectResponse' in data:
                record(session, data['requestId'], data['redirectResponse'], b'')
            requests[(session, data['requestId'])] = data['request']
        elif method == 'Network.responseReceived':
            pending[(session, data['requestId'])] = data['response']
        elif method == 'Network.loadingFinished':
            response = pending.pop((session, data['requestId']), None)
        elif method == 'Network.loadingFailed':
            evidence['failures'].append(data)
        elif method == 'Runtime.exceptionThrown':
            evidence['errors'].append(data)
        elif method == 'Fetch.requestPaused':
            request = data['request']
            if args.capture:
                if 'responseStatusCode' in data:
                    if data['responseStatusCode'] in (204,304) or 300 <= data['responseStatusCode'] < 400:
                        body=b''
                    else:
                        result = pipe.call('Fetch.getResponseBody', dict(requestId=data['requestId']), session)
                        body = base64.b64decode(result['body']) if result.get('base64Encoded') else result['body'].encode()
                    requests[(session,data['requestId'])] = request
                    response=dict(url=request['url'],status=data['responseStatusCode'],
                                  headersList=data.get('responseHeaders',[]))
                    record(session,data['requestId'],response,body)
                pipe.call('Fetch.continueRequest', dict(requestId=data['requestId']), session)
                return
            key = request['method'] + ' ' + request['url']
            response = table.get(key)
            if response and any(h['name'].lower()=='vary' for h in response['headers']):
                captured={k.lower():v for k,v in response.get('requestHeaders',{}).items()}
                incoming={k.lower():v for k,v in request.get('headers',{}).items()}
                for h in response['headers']:
                    if h['name'].lower()=='vary':
                        for field in h['value'].lower().split(','):
                            field=field.strip()
                            if captured.get(field)!=incoming.get(field): response=None;break
                    if response is None:break
            if not response:
                evidence['misses'].append(key)
                pipe.call('Fetch.failRequest', dict(requestId=data['requestId'], errorReason='InternetDisconnected'), session)
                return
            body = (args.replay / 'bodies' / response['sha256']).read_bytes()
            if hashlib.sha256(body).hexdigest() != response['sha256']:
                raise RuntimeError('corrupt replay body')
            pipe.call('Fetch.fulfillRequest', dict(requestId=data['requestId'], responseCode=response['status'],
                      responseHeaders=response['headers'], body=base64.b64encode(body).decode()), session)
    def record(session, request_id, response, body):
        request = requests.get((session, request_id), {})
        if not request:
            candidates=[v for (sid,rid),v in requests.items() if rid==request_id and v.get('url')==response['url']]
            if len(candidates)==1: request=candidates[0]
        if request.get('method') not in ('GET', 'HEAD') or request.get('postData'):
            evidence['failures'].append(dict(url=response['url'], error='unsupported request variant',method=request.get('method')))
            return
        if response['status'] >= 400:
            evidence['failures'].append(dict(url=response['url'], error='HTTP failure', status=response['status']))
        headers = []
        unsupported=False
        for header in response.get('headersList', [dict(name=k,value=v) for k,v in response.get('headers',{}).items()]):
            name,value=header['name'],header['value']
            if name.lower() == 'set-cookie' or (name.lower() == 'vary' and value.lower().strip() != 'accept-encoding'):
                evidence['failures'].append(dict(url=response['url'], error='session/variant requires explicit model', header=name))
                unsupported=True
            # CDP returns decoded bytes. Preserve security/end-to-end fields,
            # remove only transport encoding/length of the wire representation.
            if name.lower() not in ('content-encoding', 'transfer-encoding', 'content-length'):
                headers.extend(dict(name=name, value=v) for v in value.split('\n'))
        if unsupported:return
        if len(body)>32*1024*1024: raise RuntimeError('capture body exceeds trial limit')
        digest = hashlib.sha256(body).hexdigest()
        (args.capture / 'bodies' / digest).write_bytes(body)
        key = request.get('method', 'GET') + ' ' + response['url']
        if key not in table and len(table)>=4096: raise RuntimeError('capture entry limit exceeded')
        table[key] = dict(status=int(response['status']), headers=headers, sha256=digest,
                          requestHeaders=request.get('headers', {}), size=len(body))
        evidence['responses'].append(dict(key=key, sha256=digest, size=len(body)))
    pipe = Pipe(args.engine, flags, log, prefix)
    pipe.handler = handle
    try:
        evidence['engine'] = pipe.call('Browser.getVersion')
        evidence['executableSha256']=hashlib.file_digest(open(args.engine,'rb'),'sha256').hexdigest()
        pipe.call('Target.setAutoAttach', dict(autoAttach=True, waitForDebuggerOnStart=True, flatten=True))
        pages = pipe.call('Target.getTargets')['targetInfos']
        target = next(x for x in pages if x['type'] == 'page')
        session = sessions.get(target['targetId']) or pipe.call('Target.attachToTarget', dict(targetId=target['targetId'], flatten=True))['sessionId']
        pipe.call('Emulation.setDeviceMetricsOverride', dict(width=1920, height=1080, deviceScaleFactor=1, mobile=False), session)
        pipe.call('Page.enable', session=session)
        pipe.call('Page.navigate', dict(url=args.url), session)
        end = time.monotonic() + args.seconds
        while time.monotonic() < end:
            pipe.call('Runtime.evaluate', dict(expression='0'), session)
            time.sleep(.2)
        observation = pipe.call('Runtime.evaluate', dict(expression='({url:location.href,origin:location.origin,secure:isSecureContext,webgpu:!!navigator.gpu,canvas:[...document.querySelectorAll("canvas")].map(x=>[x.width,x.height]),text:document.body.innerText.slice(0,2000)})', returnByValue=True), session)
        evidence['page'] = observation.get('result', {}).get('value')
        evidence['processes']=process_evidence(args.engine)
        evidence['gpu']=pipe.call('SystemInfo.getInfo').get('gpu')
        for index in range(2):
            image = pipe.call('Page.captureScreenshot', dict(format='png'), session)
            body = base64.b64decode(image['data'])
            (args.output / f'frame-{index}.png').write_bytes(body)
            time.sleep(2)
        if args.capture:
            (args.capture / 'responses.json').write_text(json.dumps(table, indent=2))
        evidence['captureComplete'] = not evidence['failures'] and not evidence['errors']
    except Exception as error:
        evidence['trialFailure']=dict(type=type(error).__name__,message=str(error))
        raise
    finally:
        (args.output / 'result.json').write_text(json.dumps(evidence, indent=2))
        pipe.close()

if __name__ == '__main__':
    main()
