import json, statistics, sys
from pathlib import Path
from PIL import Image, ImageChops, ImageStat
d=Path(sys.argv[1])
result={}
for name in ['direct1','host1','host2','direct2']:
    samples=json.loads((d/f'{name}.json').read_text())
    low,high=samples[0],samples[-1]
    sec=(high['ns']-low['ns'])/1e9
    feedback=json.loads((d/f'{name}.timing.json').read_text())
    feedback=feedback.get('waylandPresentation',feedback)
    assert feedback['available'] and feedback['clockId']==1 and not feedback['truncatedTimestamps']
    alltimes=feedback['changedTimestampsNs']
    assert alltimes[0] <= low['ns'] and alltimes[-1] >= high['ns']-150_000_000
    t=[t for t in alltimes if low['ns']<=t<=high['ns']]
    v=sorted((b-a)/1e6 for a,b in zip(t,t[1:]))
    assert min(v)>0
    dec=(high['drmDecodeNs']-low['drmDecodeNs'])/1e9
    assert dec>0 and all(s['props']['hwdec-current']=='vaapi' and s['props']['video-params/pixelformat']=='vaapi' for s in samples)
    assert all(s['props']['current-ao']=='pulse' for s in samples)
    for first, last in zip(samples,samples[1:]):
        assert 8 < last['props']['time-pos']-first['props']['time-pos'] < 12
        assert last['drmDecodeNs'] > first['drmDecodeNs']
    a,b=(Image.open(d/f'{name}-{i}.png').convert('RGB') for i in [1,2])
    assert a.size == b.size == (480,480)
    assert a.getbbox() == b.getbbox() == (0,104,480,376)
    motion=ImageStat.Stat(ImageChops.difference(a,b)).mean
    assert min(motion)>0 and min(ImageStat.Stat(a).stddev)>10
    result[name]=dict(seconds=sec, swaps=len(t),swapMeanMs=statistics.mean(v),p95Ms=v[int(.95*len(v))],p99Ms=v[int(.99*len(v))],maxMs=max(v),over50_5ms=sum(x>50.5 for x in v),cpuPercent=(high['cpuUsec']-low['cpuUsec'])/sec/10000,meanMemoryMiB=statistics.mean(s['mem']/1048576 for s in samples),decodeEngineSeconds=dec,dropDeltas={key:[b['props'][key]-a['props'][key] for a,b in zip(samples,samples[1:])] for key in ['frame-drop-count','decoder-frame-drop-count']},captureRGBMeanAbsoluteDifferences=motion)
print(json.dumps(result,indent=2))
