# PhotoBeam 项目交接（ZCode → MiniMax Code）

## 你接手的工作目录与状态

- 工作目录：`C:\Users\<user>\Project\PhotoBeam`（下文相对路径基于此）
- **交付已完成并经你方三轮签收**：Stage 0/1/2/3 全部通过，最后一轮 `STAGE3-VERDICT: 通过`（评分满分），
  详见 `docs/delivery-status.md`、`docs/stage3-review.md`、`.lam/replies/20261102-035502-5f6c.json`
- 交付物：`dist/PhotoBeam.exe`（4.82MB GUI 子系统）+ `PhotoBeam.apk`（release 签名 CN=PhotoBeam，已装测试机）

## 关键资产与纪律（不可丢）

1. **签名钥匙**：`%LOCALAPPDATA%\PhotoBeam\photobeam-release.keystore` + `keystore.pass`（仓库外！）
   丢了就无法对已装设备覆盖升级。构建时口令优先取环境变量 `PHOTOBEAM_KEYSTORE_PASS`。
2. **审核纪律**：你施工 → 你自己留证据 → 交 MiniMax 审核签收后才算阶段完成。证据写 `docs/stage*-evidence.md`，
   审核报告 `docs/stage*-review.md`，LAM 派发模板见 `.lam/task-stage3-review.md`。
3. **协议**：`docs/protocol-v1.md`（v1.3）是唯一依据。
4. **构建**：`build-pc.bat`（PC，含 vet 门禁 + PE 校验）/ `build-android.bat`（APK，含三步校验）。
5. **LAM 投递**：`node C:\Users\<user>\.agents\skills\lam\bin\lam.js send --agent minimax-code --workdir <目录> --task-file <任务> --allow-unregistered --no-wait`。
   若 M-G0 门禁报"正在流式生成中"且持续 >30 分钟，可用 CDP（http://127.0.0.1:9666/json 找 target）reload 页面恢复。

## 你上一轮（图标/托盘/弹窗）做到哪了

用户验收点 4 个，进度如下：

| 项 | 状态 |
|----|------|
| APK 应用图标（空心 P 自适应图标） | ✅ 已装真机验证（桌面截图确认） |
| 通知图标 = 白色空心 P 剪影（弃系统箭头） | ✅ 状态栏确认（dumpsys: photobeam_fg_on_v2 importance=LOW 不响铃） |
| 传输中才动的动画（4 帧 ic_stat_upload + setUploading 挂在 flush 首尾） | ⚠ 代码已实现，**未做视觉验证**（帧动画 180ms/帧，截图时机难抓） |
| PC 托盘图标 | ✅ SetIcon 已补，Win32 枚举确认在溢出区，tooltip 正常（用户可拖到常驻区） |
| PC 弹窗缩略图（左侧 76px StretchDIBits 画 DIB） | ❌ **未生效**——v_popup2.png 只有文字没有图 |
| PC 弹窗文件名省略号 | ❌ 未生效——flags 写了 0x5024，其中 0x1000 是笔误，应为 0x4024；且需确认 DT_END_ELLIPSIS 实际画出 … |

## 待办（按优先级）

1. **弹窗缩略图调试**：`pc/popup_windows.go` 的 `StretchDIBitsTopDown`（写负 biHeight 原地改 dib[8:12]）没画出来。
   建议顺序：先在 WM_PAINT 打日志确认 `popupThumb.has==true` → 打印 StretchDIBits 返回值 →
   怀疑点是原地改 biHeight 与 GDI 拷贝时序、以及窗口类背景刷（CreateSolidBrush 0x1F1F1F）把图盖掉。
2. **省略号**：把 drawTextLine 的 flags 从 0x5024 改 0x4024（去掉误加的 0x1000），实测出 … 为止。
3. **传输动画验证**：连拍 3-5 张截图（flush 期间数秒），抓状态栏 P 图标对比有/无波点帧。
4. 顺带：`pc/PhotoBeam-test.exe`（--test-clipboard 自检）可复用做弹窗/管线回归。

## 注意事项（踩过的坑）

- 剪贴板是全项目最脆的部分：Win32 调用必须 LockOSThread；snapshotClipboard 绝不自行 Open/Close（1418）；
  大 DIB 写入后各剪贴板调用间保持 300ms 级间隔（否则 win32k 0xC0000005 杀进程，实测）；无主剪贴板跳过快照。
- MIUI：pm clear 前先 force-stop（否则进程不死状态不重置）；重装后 File 直读会 EACCES（fd/stream 兜底）。
- 测试机 <PHONE-MODEL> 已与 PC 配对（fp <PHONE-FP> ↔ <PC-FP>），队列干净（81 张全部 ack）。
- adb 可用；手机勿锁屏操作期间保持亮屏。

## 验收口径

改完后向用户交付：桌面+通知+托盘三处图标一致的截图、弹窗带缩略图的截图、上传动画的验证说明。
用户已确认图标设计（design/icon_preview.png），不要改设计方向。
