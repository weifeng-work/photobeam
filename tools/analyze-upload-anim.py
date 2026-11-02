# -*- coding: utf-8 -*-
"""
Analyse a device screen recording for the PhotoBeam status-bar upload animation.

For every extracted frame it locates the white "upload dots" that sit to the right of
the hollow P in the notification icon (ic_stat_up_0..3) and reports their vertical
centre.  The 4 animation frames differ only by that vertical position, so the
sequence of centres proves the 4-frame rotation.  Frames without dots = idle P.

Usage:
  python tools/analyze-upload-anim.py <video.mp4> <outDir>
"""

import os
import subprocess
import sys

import numpy as np
from PIL import Image

VIDEO = sys.argv[1]
OUT = sys.argv[2]
CROP = "62:60:108:30"      # x,y,w,h of the status-bar PhotoBeam icon region (1080x2400)
FPS = "5"

os.makedirs(OUT, exist_ok=True)
frames_dir = os.path.join(OUT, "crop")
os.makedirs(frames_dir, exist_ok=True)

# 1) 抽帧（已裁到图标区）
subprocess.run([
    "ffmpeg", "-y", "-v", "error", "-i", VIDEO,
    "-vf", "fps=%s,crop=%s" % (FPS, CROP),
    os.path.join(frames_dir, "c%04d.png"),
], check=True)

files = sorted(f for f in os.listdir(frames_dir) if f.endswith(".png"))
print("frames:", len(files))

# 2) 逐帧量测：P 本体在局部 x=22..32，波点在 x>=34
rows = []
levels = {}
for i, name in enumerate(files):
    a = np.array(Image.open(os.path.join(frames_dir, name)).convert("RGB")).astype(int)
    mn = a.min(axis=2)
    mx = a.max(axis=2)
    white = (mn > 185) & ((mx - mn) < 40)
    # 波点列（排除 P 本体）
    dots = white[:, 34:]
    ys, xs = np.where(dots)
    if len(ys) == 0:
        rows.append((name, None, 0, "IDLE-P"))
        continue
    yc = float(ys.mean())
    key = round(yc / 2.0) * 2
    if key not in levels:
        levels[key] = "L%d" % (len(levels) + 1)
    rows.append((name, yc, len(ys), levels[key]))

# 3) 打印时间线（压缩连续相同状态）
def fmt(r):
    if r[1] is None:
        return "IDLE-P"
    return "%s(y=%.1f,n=%d)" % (r[3], r[1], r[2])

print("\n=== 时间线（帧号: 状态）===")
start = 0
for i in range(1, len(rows) + 1):
    if i == len(rows) or fmt(rows[i]) != fmt(rows[start]):
        print("  %3d-%3d  %s" % (start + 1, i, fmt(rows[start])))
        start = i

print("\n=== 波点纵向位置聚类 ===")
for k in sorted(levels):
    idxs = [i for i, r in enumerate(rows) if r[1] is not None and abs(r[1] - k) < 1.5]
    print("  %s : y≈%.1f  帧数=%d" % (levels[k], k, len(idxs)))

# 4) 每个档位存一张放大代表图
for k in sorted(levels):
    pick = None
    for i, r in enumerate(rows):
        if r[1] is not None and abs(r[1] - k) < 1.5:
            pick = i
            break
    if pick is None:
        continue
    src = os.path.join(frames_dir, rows[pick][0])
    im = Image.open(src).convert("RGB")
    big = im.resize((im.width * 8, im.height * 8), Image.NEAREST)
    big.save(os.path.join(OUT, "dotlevel_%s_%s.png" % (levels[k], rows[pick][0])))

# 5) 汇总图：所有档位横向拼一张
imgs = []
for k in sorted(levels):
    f = [x for x in os.listdir(OUT) if x.startswith("dotlevel_%s" % levels[k])]
    if f:
        imgs.append((levels[k], Image.open(os.path.join(OUT, f[0]))))
if imgs:
    W = sum(i.width for _, i in imgs) + 20 * (len(imgs) + 1)
    H = max(i.height for _, i in imgs) + 20
    canvas = Image.new("RGB", (W, H), (25, 25, 25))
    x = 20
    for lbl, i in imgs:
        canvas.paste(i, (x, 10))
        x += i.width + 20
    canvas.save(os.path.join(OUT, "anim_levels_summary.png"))
    print("\nsummary ->", os.path.join(OUT, "anim_levels_summary.png"))

# 6) 文本台账
with open(os.path.join(OUT, "anim_timeline.txt"), "w", encoding="utf-8") as f:
    for r in rows:
        f.write("%s\t%s\n" % (r[0], fmt(r)))
print("timeline ->", os.path.join(OUT, "anim_timeline.txt"))

