# Stage 1 修复复核证据（第二轮，2026-11-02 0:0x 采集）

## 证据 1：构建输出（dex 特征校验通过）
```
完成: PhotoBeam.apk（dex 特征校验通过）
```
## 证据 2：装机 APK 特征
```
classes.dex 6444072B，含 retryUnready ✓
AndroidManifest（AXML UTF-16-LE）含 SPECIAL_USE ✓
empty_file 在 PC 侧 server.go（Go 代码）✓
probeLink 已按统一架构移除 ✓
```
## 证据 3：连续截图实时落盘（最近 8 条 PC 日志）
```
[2026-11-01 23:37:24] PhotoBeam 落盘完成: C:\Users\<user>\AppData\Local\PhotoBeam\Received\1793547444235_Screenshot_2026-11-01-23-36-54-324_com.photobeam.app.jpg
[2026-11-01 23:37:24] PhotoBeam 接收文件: Screenshot_2026-11-01-23-36-54-324_com.photobeam.app.jpg (150128 bytes) batch=<PHONE-FP-PREFIX>-694
[2026-11-01 23:36:54] PhotoBeam 落盘完成: C:\Users\<user>\AppData\Local\PhotoBeam\Received\1793547414223_Screenshot_2026-11-01-23-36-35-162_com.photobeam.app.jpg
[2026-11-01 23:36:54] PhotoBeam 接收文件: Screenshot_2026-11-01-23-36-35-162_com.photobeam.app.jpg (150011 bytes) batch=<PHONE-FP-PREFIX>-693
[2026-11-01 23:36:24] PhotoBeam 落盘完成: C:\Users\<user>\AppData\Local\PhotoBeam\Received\1793547384252_Screenshot_2026-11-01-23-35-56-505_com.photobeam.app.jpg
[2026-11-01 23:36:24] PhotoBeam 接收文件: Screenshot_2026-11-01-23-35-56-505_com.photobeam.app.jpg (150324 bytes) batch=<PHONE-FP-PREFIX>-692
[2026-11-01 22:02:49] PhotoBeam 落盘完成: C:\Users\<user>\AppData\Local\PhotoBeam\Received\1793541769076_Screenshot_2026-11-01-22-02-29-220_com.photobeam.app.jpg
[2026-11-01 22:02:49] PhotoBeam 接收文件: Screenshot_2026-11-01-22-02-29-220_com.photobeam.app.jpg (150946 bytes) batch=<PHONE-FP-PREFIX>-691
```

PC 接收目录文件总数: **133**

最近 3 个:
```
1793547384252_Screenshot_2026-11-01-23-35-56-505_com.photobeam.app.jpg
1793547414223_Screenshot_2026-11-01-23-36-35-162_com.photobeam.app.jpg
1793547444235_Screenshot_2026-11-01-23-36-54-324_com.photobeam.app.jpg
```
