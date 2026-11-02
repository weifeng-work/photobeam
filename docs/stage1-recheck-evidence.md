# Stage 1 修复复核证据（2026-11-01 22:02 采集）

## 证据 1：构建输出（含 classes.dex 校验）
```
[4/5] 组装 dex 进 APK
dex added
[5/5] zipalign + apksigner
完成: PhotoBeam.apk（dex 特征校验通过）
```

## 证据 2：装机 APK dex 特征串检索
```
classes.dex 大小: 6444072
retryUnready 出现
empty_file: PC 侧常量（Go server.go），不在 Android dex——server.go: handleFileMeta 已含 size<=0 → ACK permanent empty_file
PROPERTY_SPECIAL_USE: 位于 AndroidManifest.xml（二进制 AXML 字符串池，UTF-16-LE 编码）
utf-16-le SPECIAL_USE: True
probeLink: 已按统一架构移除（发送即探活）
```

## 证据 3：连续 3 次截图落盘（含 size）
```
[2026-11-01 22:02:49] PhotoBeam 落盘完成: C:\Users\<user>\AppData\Local\PhotoBeam\Received\1793541769076_Screenshot_2026-11-01-22-02-29-220_com.photobeam.app.jpg
[2026-11-01 22:02:49] PhotoBeam 接收文件: Screenshot_2026-11-01-22-02-29-220_com.photobeam.app.jpg (150946 bytes) batch=<PHONE-FP-PREFIX>-691
[2026-11-01 22:02:48] PhotoBeam 落盘完成: C:\Users\<user>\AppData\Local\PhotoBeam\Received\1793541768900_Screenshot_2026-11-01-22-02-22-769_com.photobeam.app.jpg
[2026-11-01 22:02:48] PhotoBeam 接收文件: Screenshot_2026-11-01-22-02-22-769_com.photobeam.app.jpg (150735 bytes) batch=<PHONE-FP-PREFIX>-690
[2026-11-01 22:02:18] PhotoBeam 落盘完成: C:\Users\<user>\AppData\Local\PhotoBeam\Received\1793541738906_Screenshot_2026-11-01-22-02-16-431_com.photobeam.app.jpg
[2026-11-01 22:02:18] PhotoBeam 接收文件: Screenshot_2026-11-01-22-02-16-431_com.photobeam.app.jpg (150948 bytes) batch=<PHONE-FP-PREFIX>-689
```

PC 接收目录文件总数: **130**
