# -*- coding: utf-8 -*-
"""
一次性抽取：把 design/icon_master_512.png 拆成
  design/icon_shape.png   深色圆角方块（已抹掉 P 的白像素，只留底图剪影）
  design/icon_glyph_p.png  白色空心 P 字形（RGBA，已裁到字形边界）

之后 tools/gen-app-icons.py 以这两个文件为唯一真源，新版式下幂等可重跑。
"""

import os

import numpy as np
from PIL import Image

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DESIGN = os.path.join(ROOT, "design")
SRC = os.path.join(DESIGN, "icon_master_512.png")

a = np.array(Image.open(SRC).convert("RGBA")).astype(float)
alpha = a[..., 3] / 255.0
lum = 0.299 * a[..., 0] + 0.587 * a[..., 1] + 0.114 * a[..., 2]

# 底色是 #1A1A1A（lum≈26），P 是纯白（lum=255）→ 线性反解出 P 的覆盖度
cov = np.clip((lum - 30.0) / (255.0 - 30.0), 0.0, 1.0) * alpha

# --- 底图：抹掉 P 像素，只保留深色方块剪影 ---
shape = a.copy()
is_p = cov > 0.002
shape[..., 0][is_p] = 26
shape[..., 1][is_p] = 26
shape[..., 2][is_p] = 26
shape[..., 3] = alpha * 255.0

# 原母版顶边有一小块毛刺（x≈118..179 高出平顶边 16px）→ 按平顶边裁掉
tops = []
for x in range(shape.shape[1]):
    ys = np.where(shape[:, x, 3] > 10)[0]
    tops.append(int(ys.min()) if len(ys) else 10 ** 6)
tops = np.array(tops)
flat_edge = int(np.bincount(tops[tops < 10 ** 6]).argmax())   # 平顶边 y（众数）
if (tops < flat_edge).any():
    shape[:flat_edge, :, 3] = 0
    print("裁掉顶边毛刺: y < %d，共 %d 列" % (flat_edge, int((tops < flat_edge).sum())))

ys, xs = np.where(shape[..., 3] > 0.02)
shape = shape[ys.min():ys.max() + 1, xs.min():xs.max() + 1]
Image.fromarray(shape.astype("uint8"), mode="RGBA").save(os.path.join(DESIGN, "icon_shape.png"))
print("icon_shape.png:", shape.shape[1], "x", shape.shape[0])

# --- 字形：P 的覆盖度做 alpha，裁到字形边界 ---
ys, xs = np.where(cov > 0.02)
g = np.zeros((ys.max() - ys.min() + 1, xs.max() - xs.min() + 1, 4), dtype=float)
g[..., 0] = 255
g[..., 1] = 255
g[..., 2] = 255
g[..., 3] = cov[ys.min():ys.max() + 1, xs.min():xs.max() + 1] * 255.0
Image.fromarray(g.astype("uint8"), mode="RGBA").save(os.path.join(DESIGN, "icon_glyph_p.png"))
print("icon_glyph_p.png:", g.shape[1], "x", g.shape[0], "  aspect w/h = %.3f" % (g.shape[1] / g.shape[0]))
