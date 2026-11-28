#!/usr/bin/env python3
"""Validate synthetic native image output, including EXIF, alpha and animation."""
import json
from pathlib import Path
import sys
from PIL import Image
root=Path(sys.argv[1]);checks=[]
def near(pixel,expected,tolerance=6):
    return max(abs(a-b) for a,b in zip(pixel[:3],expected))<=tolerance
image=Image.open(root/'exif-rotate-6-0.png').convert('RGB');w,h=image.size
assert near(image.getpixel((w//2,h//4)),(255,0,0)), 'EXIF 6 top half must be red'
assert near(image.getpixel((w//2,3*h//4)),(0,0,255)), 'EXIF 6 bottom half must be blue'
checks.append('EXIF orientation 6 pixels')
image=Image.open(root/'alpha-png-0.png').convert('RGB');w,h=image.size
assert near(image.getpixel((w//2,h//2)),(127,127,255),2),'50% blue alpha over white'
assert near(image.getpixel((1,1)),(255,255,255),1),'Contain framing leaves white margins'
checks.append('Straight alpha composition and framing')
image=Image.open(root/'alpha-avif-0.png').convert('RGB');w,h=image.size
assert near(image.getpixel((w//2,h//2)),(127,127,255),6),'AVIF auxiliary alpha must blend over white'
checks.append('AVIF auxiliary alpha')
colors=[Image.open(p).convert('RGB').getpixel((w//2,h//2)) for p in sorted(root.glob('animated-gif-*.png'))]
assert any(near(p,(255,0,0)) for p in colors) and any(near(p,(0,255,0)) for p in colors),'GIF must show both animation frames'
checks.append('GIF animation advances through both colors')
result={'result':'passed','checks':checks,'gifSamples':colors,'capture':'Compositor output; no physical-panel claim'}
(root/'pixel-check.json').write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
