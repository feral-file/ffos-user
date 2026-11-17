#!/usr/bin/env python3
"""Canonical v1 wire schema. Regenerate/check without runtime dependencies."""
import argparse
import json
from pathlib import Path

def obj(properties=None, required=()):
    return {'type':'object','properties':properties or {},'required':list(required),'additionalProperties':False}

def text(limit=4096):
    return {'type':'string','maxLength':limit,'description':f'Wire limit is {limit} UTF-8 bytes, additionally enforced by peers.'}

def number(low, high, integer=False):
    return {'type':'integer' if integer else 'number','minimum':low,'maximum':high}

def enum(*values):
    return {'enum':list(values)}

def ref(name):
    return {'$ref':'#/$defs/'+name}

def nullable(value):
    return {'anyOf':[value,{'type':'null'}]}

identifier={**text(64),'minLength':1}
boolean={'type':'boolean'}
uint={'type':'integer','minimum':0,'maximum':18446744073709551615}
empty=obj()
context=enum('curated','personal')
settings=obj({
    'scaling':enum('fit','fill','stretch','auto'),'framing':enum('artwork','fit','fill'),
    'tombstone':enum('on','off','timed'),'transitionMs':number(0,5000),
    'durationSeconds':nullable({'type':'number','exclusiveMinimum':0,'maximum':86400000}),
    'margin':number(0,86400000),'background':{'type':'string','pattern':'^#[0-9a-fA-F]{6}$'},
    'rotation':enum(0,90,180,270),'volume':number(0,1),'muted':boolean})
policy=obj({'version':{'const':1},'showMatureContent':boolean,'strictPersonal':boolean,'blockUnratedCurated':boolean},
           ('version','showMatureContent','strictPersonal','blockUnratedCurated'))
display={'type':'object','properties':{'scaling':enum('fit','fill','stretch','auto'),
    'margin':{'anyOf':[number(0,16384),{'type':'string','maxLength':16,'pattern':'%$','description':'Finite percentage 0..100, parsed and range-checked by the host.'}]},
    'background':text(64),'autoPlay':boolean,'loop':boolean,'userOverrides':boolean},'additionalProperties':True}
item={'type':'object','required':['source'],'properties':{'source':{**text(16384),'pattern':'^(https?://|data:|ipfs://|onchfs://)'},
    **{key:text() for key in ('id','title','slug','license','ref','refHash')},'mimeType':text(256),'duration':{'type':'number','minimum':0},
    'contentRating':{'type':'string'},'contentReasons':{'type':'array','items':{'type':'string'}},'display':ref('display'),
    'inlineManifest':{'type':'object'},'resolvedManifest':{'type':'object'},'override':{'type':'object'},'metadata':{'type':'object'}},
    'additionalProperties':True,'description':'DP-1 document with extensions preserved; admission limit 64 KiB per item.'}
playlist={'type':'object','required':['items'],'properties':{'items':{'type':'array','maxItems':4096,'items':ref('item')},
    **{key:text() for key in ('id','title','curator','dpVersion','slug','created')},'defaults':{'type':'object'}},'additionalProperties':True,'description':'DP-1 document, at most 1 MiB encoded.'}
inputs=[]
for kind,properties,required in [
    ('move',{key:number(-16384,16384) for key in ('x','y','dx','dy')},()),
    ('button',{'button':number(1,3,True),'clicks':number(1,3,True),'down':boolean},('down',)),
    ('wheel',{key:number(-32768,32768,True) for key in ('dx','dy')},()),
    ('key',{'usage':number(1,255,True),'down':boolean},('usage','down')),
    ('text',{'text':{**text(4096),'minLength':1}},('text',)),('releaseAll',{},())]:
    inputs.append(obj({'kind':{'const':kind},'occurrence':identifier,**properties},('kind','occurrence',*required)))

methods={
    'hello':obj({'client':identifier,'versions':{'type':'array','minItems':1,'maxItems':16,'items':number(1,65535,True)}},('client','versions')),
    **{name:empty for name in ('state.get','health.get','state.subscribe','policy.get','history.list','playback.refresh','playback.stop','playback.cancel')},
    'history.resolve':obj({'recordId':identifier},('recordId',)),
    'playback.replace':obj({'playlist':ref('playlist'),'index':number(0,4096,True),'context':context,'playlistUrl':text(16384)},('playlist',)),
    'playback.update':obj({'items':{'type':'array','maxItems':4096,'items':ref('item')},'context':context,'retireBlockedCurrent':boolean},('items',)),
    'playback.select':obj({'index':number(0,4096,True)},('index',)),
    'playback.pause':obj({'paused':boolean},('paused',)),
    'playback.sleep':obj({'sleeping':boolean},('sleeping',)),
    'playback.mode':obj({'loop':enum('none','playlist','one'),'shuffle':boolean}),
    'media.seek':obj({'occurrence':identifier,'seconds':number(0,86400000)},('occurrence','seconds')),
    'settings.set':ref('settings'),'policy.set':ref('policy'),
    'connectivity.set':obj({'online':boolean},('online',)),
    'overlay.set':obj({'owner':enum('setup','pairing','recovery'),'state':identifier,'fields':{'type':'object','additionalProperties':True}},('owner','state')),
    'input.dispatch':{'oneOf':inputs},
}
setup_fields=obj({**{k:text(2048) for k in ('reason','ssid','password','portal_url','url','device_name')},'client_attached':boolean,'progress':{'type':'number'}})
setup_fields['properties']['password']=text(128)
methods['overlay.set']['allOf']=[{'if':{'properties':{'owner':{'const':'setup'}}},'then':{'properties':{
 'state':enum('softap_qr','joining','join_failed','updating','claim_qr','ready','hidden','scanning','finalizing','factory_reset','connecting','setup_error'),'fields':setup_fields}}}]

for name in ('schedule.set','boot.set'):
    properties={'playlist':ref('playlist'),'context':context,'atMs':{'type':'integer','minimum':1},'clear':boolean}
    value=obj(properties)
    value['allOf']=[{'if':{'properties':{'clear':{'const':True}},'required':['clear']},'else':{'required':['playlist']+(['atMs'] if name=='schedule.set' else [])}}]
    methods[name]=value

request=obj({'v':{'const':1},'id':identifier,'method':{'type':'string','enum':list(methods)},'params':{'type':'object'},
    'instance':identifier,'generation':uint,'ifRevision':uint,'deadlineMs':{'type':'integer'},'key':identifier},('v','id','method'))
queries=('state.get','health.get','state.subscribe','policy.get','history.list','history.resolve')
request['allOf']=[
    {'if':{'properties':{'method':{'not':{'const':'hello'}}}},'then':{'required':['deadlineMs']}},
    {'if':{'properties':{'method':{'not':enum('hello',*queries)}}},'then':{'required':['instance','generation','key']}},
]
request['oneOf']=[{'properties':{'method':{'const':name},'params':params},
                  'required':['params'] if params.get('required') or name in ('input.dispatch','policy.set','schedule.set','boot.set') else []}
                 for name,params in methods.items()]
occurrence=obj({'id':identifier,'generation':uint,'index':number(0,4095,True),'item':ref('item'),'display':ref('display'),
    'stage':enum('loading','committed','presented','failed'),'error':{'type':'string'},'navigationCommitted':boolean,
    'context':context,'exhibition':text(),'curator':text()},('id','generation','index','item','display','stage','navigationCommitted'))
snapshot=obj({'instance':identifier,'generation':uint,'revision':uint,'host':enum('responsive'),
    'renderer':enum('absent','loading','committed','presented','failed'),
    'selected':nullable(ref('occurrence')),'presented':nullable(ref('occurrence')),
    'playlist':nullable(ref('playlist')),'playlistUrl':{'type':'string'},'presentedPlaylist':nullable(ref('playlist')),'presentedPlaylistUrl':{'type':'string'},
    'stopped':boolean,'sleeping':boolean,'paused':boolean,'mediaPaused':boolean,'loop':enum('none','playlist','one'),'shuffle':boolean,
    'online':boolean,'context':context,'settings':ref('settings'),'overlay':nullable({'type':'object'}),'media':nullable({'type':'object'}),
    'schedule':nullable({'type':'object'}),'scheduleOutcome':nullable(ref('scheduleOutcome')),'recovery':ref('recovery'),
    'pendingUpdate':nullable(obj({'id':identifier,'itemCount':number(0,4096,True)},('id','itemCount'))),
    'updateOutcome':nullable(obj({'id':identifier,'outcome':enum('deferred','applied','failed'),'contentBlocked':boolean},('id','outcome','contentBlocked'))),
    'presentationFailure':nullable(obj({'occurrence':identifier,'generation':uint,'error':{'type':'string'}},('occurrence','generation','error'))),
    'storage':obj({'health':enum('available','write_failed'),'error':{'type':'string'}},('health','error'))},
    ('instance','generation','revision','host','renderer','selected','presented','playlist','presentedPlaylist','settings','storage'))
# Readers tolerate additive snapshot fields within a major version.
snapshot['additionalProperties']=True
response=obj({'v':{'const':1},'id':nullable(identifier),'result':{},'error':obj({'code':identifier,'message':{'type':'string'}},('code','message'))},('v','id'))
response['oneOf']=[{'required':['result'],'not':{'required':['error']}},{'required':['error'],'not':{'required':['result']}}]
event=obj({'v':{'const':1},'event':{'const':'state'},'previous':uint,'cursor':uint,'snapshot':ref('snapshot')},('v','event','previous','cursor','snapshot'))
receipt=obj({'outcome':enum('accepted','applied'),'generation':uint,'revision':uint,'occurrence':nullable(identifier),'updateId':identifier},('outcome',))
hello=obj({'version':{'const':1},'instance':identifier,'maxFrameBytes':{'const':4194304},'receiptWindow':{'const':1024},
    'capabilities':{'type':'array','items':{'type':'string'}},'snapshot':ref('snapshot')},
    ('version','instance','maxFrameBytes','receiptWindow','capabilities','snapshot'))
subscription=obj({'snapshot':ref('snapshot'),'cursor':uint},('snapshot','cursor'))
record={'type':'object','properties':{'recordId':identifier,'playedAtMs':{'type':'integer'},'item':ref('item'),'context':context,'defaults':{'type':'object'}},
    'required':['recordId','playedAtMs','item','context'],'additionalProperties':True}
history=obj({'records':{'type':'array','maxItems':100,'items':ref('record')},'incomplete':boolean,'activeOccurrence':nullable(identifier)},
    ('records','incomplete','activeOccurrence'))
schedule_outcome=obj({'id':identifier,'atMs':uint,'executedAtMs':uint,'instance':identifier,'generation':uint,
    'outcome':enum('applied','failed'),'error':obj({'code':identifier,'message':text()},('code','message'))},
    ('id','atMs','executedAtMs','instance','generation','outcome'))
schedule_outcome['allOf']=[{'if':{'properties':{'outcome':{'const':'failed'}}},'then':{'required':['error']}}]
recovery=obj({'attempt':number(0,3,True),'limit':{'const':3},'pending':boolean,'exhausted':boolean,
    'lastError':{'type':'string'},'failedOccurrence':{'type':'string'},'retryDelayMs':enum(0,1000,2000,4000)},
    ('attempt','limit','pending','exhausted','lastError','failedOccurrence','retryDelayMs'))
schema={'$schema':'https://json-schema.org/draft/2020-12/schema','$id':'urn:feral-file:player-protocol:1',
    'title':'Feral File native player local protocol v1',
    'description':'Framing, UTF-8 byte/depth limits, duplicate-key rejection, deadlines, live identity and transition rules are specified in README.md and enforced by peers. JSON Schema integer accepts integral decimal lexemes; the wire additionally requires integer JSON lexemes for integer fields.',
    'oneOf':[ref('request'),ref('response'),ref('event')],
    '$defs':{'request':request,'response':response,'event':event,'snapshot':snapshot,'occurrence':occurrence,
             'settings':settings,'policy':policy,'display':display,'playlist':playlist,'item':item,
             'receipt':receipt,'hello':hello,'subscription':subscription,'record':record,'history':history,'scheduleOutcome':schedule_outcome,'recovery':recovery}}

parser=argparse.ArgumentParser();parser.add_argument('--check',action='store_true');args=parser.parse_args()
path=Path(__file__).with_name('schema.json');encoded=json.dumps(schema,indent=2)+'\n'
if args.check:
    if path.read_text()!=encoded: raise SystemExit('schema.json differs from canonical generator')
else: path.write_text(encoded)
print('Protocol schema is current')
