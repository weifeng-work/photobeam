# -*- coding: utf-8 -*-
"""
Generate N content-distinct test images for PhotoBeam transfer/animation testing.

Distinct content is mandatory: PhotoBeam's PC side de-duplicates by SHA-256 file_id,
so identical images would be acked as "duplicate" and never produce a fresh transfer.

Usage:
  python tools/gen-test-images.py <outDir> <count> [width] [height] [quality] [seed]
"""

import os
import sys

import numpy as np
from PIL import Image, ImageDraw

# 默认输出目录 = <项目根>\docs\stage4-screenshots\probeimgs（随脚本位置推导，不写死本机路径）
_DEFAULT_OUT = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "..", "docs", "stage4-screenshots", "probeimgs"
)
OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.normpath(_DEFAULT_OUT)
COUNT = int(sys.argv[2]) if len(sys.argv) > 2 else 40
W = int(sys.argv[3]) if len(sys.argv) > 3 else 1080
H = int(sys.argv[4]) if len(sys.argv) > 4 else 2340
Q = int(sys.argv[5]) if len(sys.argv) > 5 else 92
SEED = int(sys.argv[6]) if len(sys.argv) > 6 else 20261102

os.makedirs(OUT, exist_ok=True)
rng = np.random.default_rng(SEED)

for i in range(1, COUNT + 1):
    # Coarse noise + per-image unique marker => distinct SHA-256 per file.
    small = rng.integers(0, 256, size=(H // 6 + 1, W // 6 + 1, 3), dtype=np.uint8)
    im = Image.fromarray(
        np.repeat(np.repeat(small, 6, axis=0), 6, axis=1)[:H, :W]
    )
    d = ImageDraw.Draw(im)
    d.rectangle([40, 40, 260 + 60 * (i % 3), 200], fill=(255, 255, 255))
    d.text((60, 100), "PB%03d" % i, fill=(0, 0, 0))
    name = "PhotoBeam_s%d_%03d_2026-11-02_06-45-00-000.jpg" % (SEED, i)
    im.save(os.path.join(OUT, name), "JPEG", quality=Q)

print("generated %d distinct images in %s" % (COUNT, OUT))
