# -*- coding: utf-8 -*-
"""
PhotoBeam 应用图标统一生成器（启动器 / 托盘 / 设计母版同源同版式）

[2026-11-02 用户反馈驱动]
  旧版式把空心 P "顶端出头挂在左上角"（源自 design/icon_master_512.png 的原始构图），
  在两处都表现不好：
    - 手机桌面（自适应图标）：P 画在 108dp 原始画布上，而启动器只露中心 72dp，
      P 顶部约 1/3 被蒙版裁掉；即便修正位置，P 只占可见区高度 52%，实机偏小；
    - 电脑托盘：icon.ico 里 P 的中心明显偏左上（16px 时 P 中心 (5.5,3.5)，
      画布中心 (7.5,7.5)），不是正中间。
  → 新版式：**P 在图标内水平+垂直居中，高度占图标 66%**，字形本身不变。

版式（唯一真源，本文件 + design/icon_glyph_p.png + design/icon_shape.png）：
  - 底图：深色圆角方块 #1A1A1A（来自 design/icon_shape.png）
  - 字形：白色空心 P（来自 design/icon_glyph_p.png，字形不重画）
  - 布局：P 水平居中、垂直居中，高度 = 图标高度 × 0.66

产物：
  design/icon_master_512.png          512 母版（新版式）
  design/icon_preview.png             设计总览（应用/通知/动画帧）
  pc/icon.ico                         PC 托盘图标（7 种尺寸）
  android/.../mipmap-*/ic_launcher.png            传统图标（5 密度）
  android/.../mipmap-*/ic_launcher_foreground.png 自适应前景（5 密度，P 相对蒙版可见区居中）

用法：
  python tools/gen-app-icons.py
"""

import os
import sys

import numpy as np
from PIL import Image, ImageDraw

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DESIGN = os.path.join(ROOT, "design")
RES = os.path.join(ROOT, "android", "app", "src", "main", "res")
PC = os.path.join(ROOT, "pc")

GLYPH = os.path.join(DESIGN, "icon_glyph_p.png")   # 白色空心 P（RGBA，已裁到字形边界）
SHAPE = os.path.join(DESIGN, "icon_shape.png")     # 深色圆角方块（RGBA，已裁到形状边界）

BG = (26, 26, 26, 255)          # #1A1A1A
FG = (255, 255, 255, 255)
P_RATIO = 0.66                  # P 高度 / 图标高度（传统图标 / PC 托盘，无蒙版约束）
# 自适应前景：MIUI 会把前景层再缩放（实测渲染出的 P 只有图标高度的 34%，
# 而源文件里 P 占 72dp 可见区的 66%），所以前景要画得更大才够看。
# 上限受 66dp 安全圆约束：P 角点到中心的距离 = 0.543*P高 ≤ 33dp → P高 ≤ 60.8dp
# = 可见区的 84%。取 0.78（=56dp）留出余量。
FG_RATIO = 0.78
ICO_SIZES = [16, 24, 32, 48, 64, 128, 256]
DENSITIES = [("mdpi", 1.0), ("hdpi", 1.5), ("xhdpi", 2.0), ("xxhdpi", 3.0), ("xxxhdpi", 4.0)]


def load_glyph():
    """字形 alpha（P 笔画覆盖度 0..1）。"""
    a = np.array(Image.open(GLYPH).convert("RGBA"), dtype=float)
    return a[..., 3] / 255.0


def load_shape():
    return Image.open(SHAPE).convert("RGBA")


def glyph_mask(size, ratio=P_RATIO):
    """按目标高度缩放字形，返回 (mask, w, h)。"""
    gh, gw = load_glyph().shape
    h = max(1, int(round(size * ratio)))
    w = max(1, int(round(h * gw / gh)))
    m = Image.fromarray((load_glyph() * 255).astype("uint8"), mode="L").resize((w, h), Image.LANCZOS)
    return np.array(m, dtype=float) / 255.0, w, h


def render_icon(size, ratio=P_RATIO, shape=None):
    """深色底图 + 居中的白色空心 P → RGBA 图标。"""
    shape = shape or load_shape()
    base = np.array(shape.resize((size, size), Image.LANCZOS)).astype(float)
    mask, w, h = glyph_mask(size, ratio)
    x0 = (size - w) // 2
    y0 = (size - h) // 2
    region = base[y0:y0 + h, x0:x0 + w, :3]
    m = mask[..., None]
    base[y0:y0 + h, x0:x0 + w, :3] = region * (1 - m) + np.array(FG[:3], dtype=float) * m
    return Image.fromarray(base.astype("uint8"), mode="RGBA")


def render_foreground(canvas_size, ratio=FG_RATIO):
    """
    自适应图标前景：108dp 画布，只有中心 72dp 会被启动器蒙版露出。
    P 必须相对「可见区」居中（P 的中心 = 画布中心 = 可见区中心）。
    """
    vis = canvas_size / 3.0            # 72dp 可见区
    m = Image.new("RGBA", (canvas_size, canvas_size), (0, 0, 0, 0))
    mask, w, h = glyph_mask(int(round(vis)), ratio)
    x0 = int(round((canvas_size - w) / 2.0))
    y0 = int(round((canvas_size - h) / 2.0))
    white = Image.new("RGBA", (w, h), FG)
    m.paste(white, (x0, y0), Image.fromarray((mask * 255).astype("uint8"), mode="L"))
    return m


def save_ico(path, sizes=ICO_SIZES):
    """
    手工写 PNG 条目的 ICO（PIL 的 ICO 插件在 append_images 下只写一个条目）。
    结构：ICONDIR(6) + ICONDIRENTRY(16×N) + 各帧 PNG 数据。
    与原 icon.ico 一致（原始文件也是 PNG 条目：7 尺寸共 10991 B）。
    """
    import struct

    pngs = []
    for s in sizes:
        buf = os.path.join(ROOT, ".ico_tmp_%d.png" % s)
        render_icon(s).save(buf, format="PNG")
        pngs.append((s, open(buf, "rb").read()))
        os.remove(buf)

    out = bytearray()
    out += struct.pack("<HHH", 0, 1, len(pngs))
    offset = 6 + 16 * len(pngs)
    for s, data in pngs:
        out += struct.pack("<BBBBHHII", s if s < 256 else 0, s if s < 256 else 0,
                           0, 0, 1, 32, len(data), offset)
        offset += len(data)
    for s, data in pngs:
        out += data
    with open(path, "wb") as f:
        f.write(bytes(out))
    print("  ICO 条目: %s  总字节 %d" % (
        ", ".join("%dx%d" % (s, s) for s in sizes), len(out)))


def build_preview(icon_p, icon_p_off, up_frames):
    """设计总览：应用图标 / 通知图标 / 动画 4 帧。"""
    cell = 220
    pad = 28
    W = cell * 2 + pad * 3
    H = cell + pad * 2 + 40
    c = Image.new("RGB", (W, H), (240, 240, 240))
    d = ImageDraw.Draw(c)

    def paste_center(im, slot):
        x = pad + slot * (cell + pad) + (cell - im.width) // 2
        y = pad + (cell - im.height) // 2
        c.paste(im, (x, y), im)

    paste_center(icon_p, 0)
    paste_center(icon_p_off, 1)
    x = pad + 2 * (cell + pad)
    for i, f in enumerate(up_frames):
        y = pad + i * (cell // 4)
        c.paste(f.resize((cell // 4, cell // 4), Image.LANCZOS), (x, y), f.resize((cell // 4, cell // 4), Image.LANCZOS))
    d.text((pad, 8), "PhotoBeam icons (P centered, 66% height)", fill=(20, 20, 20))
    d.text((pad, H - 26), "app icon", fill=(60, 60, 60))
    d.text((pad + cell + pad, H - 26), "notification", fill=(60, 60, 60))
    d.text((x, H - 26), "upload frames", fill=(60, 60, 60))
    return c


def main():
    shape = load_shape()
    print("glyph/shape 载入完成")

    # 1) 设计母版（512）
    master = render_icon(512, shape=shape)
    master.save(os.path.join(DESIGN, "icon_master_512.png"))
    print("design/icon_master_512.png 已重生成（新版式）")

    # 2) PC 托盘图标
    save_ico(os.path.join(PC, "icon.ico"))
    print("pc/icon.ico 已重生成：%s" % ", ".join("%dx%d" % (s, s) for s in ICO_SIZES))

    # 3) Android 各密度
    for bucket, scale in DENSITIES:
        d = os.path.join(RES, "mipmap-" + bucket)
        os.makedirs(d, exist_ok=True)
        legacy = render_icon(int(round(48 * scale)), shape=shape)
        legacy.save(os.path.join(d, "ic_launcher.png"))
        fg = render_foreground(int(round(108 * scale)))
        fg.save(os.path.join(d, "ic_launcher_foreground.png"))
        print("mipmap-%-8s 传统 %dx%d  前景 %dx%d" % (
            bucket, legacy.width, legacy.height, fg.width, fg.height))

    # 4) 设计总览
    dr = os.path.join(RES, "drawable")
    icon_p = Image.open(os.path.join(dr, "ic_stat_p.png")).convert("RGBA")
    icon_off = Image.open(os.path.join(dr, "ic_stat_p_off.png")).convert("RGBA")
    ups = [Image.open(os.path.join(dr, "ic_stat_up_%d.png" % i)).convert("RGBA") for i in range(4)]
    build_preview(icon_p, icon_off, ups).save(os.path.join(DESIGN, "icon_preview.png"))
    print("design/icon_preview.png 已重生成")


if __name__ == "__main__":
    main()
