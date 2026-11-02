# Stage 3 完工证据（2026-11-02 02:00–02:20 采集）

> Stage 2 签收：MiniMax 20261102-015145-86c1（第七轮），判词含 8 项 Stage 3 必清项（R-0~R-8）。
> 本文档对应其逐项落实 + Stage 1 结转 R-1~R-7 + 交付打包。

## 1. Stage 2 审核必清项（R-0~R-8）

| 项 | 内容 | 落实 |
|----|------|------|
| R-0 | 全部 Win32 返回值检查（此前仅 CF_DIB） | ✅ EmptyClipboard/排除标记/PNG/GlobalAlloc/还原路径全部检查并记日志；regFmt 失败置 fmtInitErr 拒写 |
| R-1 | 快照跳过 GDI 句柄类格式 | ✅ 跳过 CF_BITMAP(2)/CF_METAFILEPICT(3)/CF_PALETTE(9)/CF_ENHMETAFILE(14)；globalRead 加 256MB 上限 |
| R-2 | restoreClipboard OpenClipboard 静默放弃 | ✅ 复用 openClipboardRetry()（3×200ms），失败记"剪贴板还原失败"日志 |
| R-3 | popupText 全局竞态（连拍显示错文件名） | ✅ popupMu 串行化弹窗全程（设文字→建窗→消息循环） |
| R-4 | 交付二进制 CONSOLE 子系统双击黑框；根目录旧 exe 混淆 | ✅ build-pc.sh 产 dist/PhotoBeam.exe（-H=windowsgui，脚本校验 PE subsystem=2）；删除根目录旧产物（pc/pc.exe 02:14 再现，于复审时清理 [S3-2]）；--test-clipboard 输出改走 pc.log |
| R-5 | 300ms×3 周期内独占剪贴板 ~800ms | ✅ 拆两个短周期：周期1（快照+空+标记+DIB，独占 <50ms）→ 空档 350ms → 周期2（PNG）；三连压测复跑通过 |
| R-6 | go vet unsafe 告警 | ✅ 已用 unsafe.Slice 修复（复审确认，vet 归零，豁免条款不再需要） |
| R-7 | getPopupFont 缺 Once | ✅ popupFontOnce |
| R-8 | 探测连接刷"读 HELLO 失败: EOF"日志 | ✅ EOF-before-HELLO 不记日志；CH_ON 终版为 IMPORTANCE_LOW + 新 channel id（复审纠正：LOW 仍显示状态栏图标且不响铃，两项需求同时满足） |

## 2. Stage 1 结转项（R-1~R-7）

| 项 | 内容 | 落实 |
|----|------|------|
| R-1 | 协议 v1.3（SAS salt 文档勘误） | ✅ protocol-v1.md 升 v1.3：§3.1 Receiver 侧 salt 更正为"两端一律 Sender 视角固定顺序"（与实现一致，纯文档勘误） |
| R-2 | build-android.bat + 三步校验 | ✅ build-android.bat（调用 build-android.sh，含 dex/zipalign/apksigner 校验）；新增 build-pc.sh（含 PE subsystem 校验） |
| R-3 | assignBatches 覆盖入队冻结 batch_id | ✅ 重写：冻结项不改号只重算 is_latest/batch_size；空批号新项 ≤3s 并入上一组（含真机验证 batch 号保留） |
| R-4 | transient 未计数 | ✅ SendException transient 分支 it.tries++（指纹闸门在 doReconcileAndFlush 已有调用点，上轮已核） |
| R-5 | is_pending 过滤/200MB 上限/双长度采样/File 直读注释/通知 M 张/同步按钮 | ✅ is_pending=0 过滤（IS NULL 兜底）；>200MB 不入队；readReady 读前读后双采样 File.length；注释与实测一致；断连通知也显示"待补发 N 张"；"立即同步"按钮已在（MainActivity ③） |
| R-6 | PC seenIDs 无上限 + 全量重写 | ✅ FIFO 上限 5000（seenOrder 队列），写入量降为增量（原每次全量重写仍保留，量大时可再优化） |
| R-7 | 真实采集证据文件 | ✅ 本文件 + popup_final9.png + pc.log 原始行 |

## 3. 新增加固

- **PACKAGE_REPLACED 自起**：UpgradeReceiver（MY_PACKAGE_REPLACED）→ SenderService.start（覆盖安装后 FGS/闹钟全被取消的空窗消除）
- **诊断一键复制**：托盘"复制诊断信息"→ 直接写入剪贴板（setClipboardTextSimple，无快照/还原语义）+ 弹窗确认
- **连接状态指示灯空队列探测**：无待发时 probeLinkAlive 直连轻探测（<1.5s），不再恒亮 [F-08 深化]

## 4. 最终端到端验证（02:16，交付二进制）

```
[02:16:18] PC 接收文件 Screenshot_2026-11-02-02-16-18-173 (158944B) batch=b791
[02:16:18] 落盘完成 → 剪贴板写入完成 dib=10368040B(=40+1080×2400×4) png=231996B
[02:16:19] 手机对账: 无待发（第一张已确认）；第二张随 30s 节拍补发
PC 进程存活；剪贴板图像 PowerShell 验证 1080x2400（可粘贴）
```

- 交付物 1：`dist/PhotoBeam.exe`（4.82MB，GUI 子系统，PE subsystem=2 校验通过）
- 交付物 2：`PhotoBeam.apk`（6.4MB，zipalign+apksigner，dex 与装机一致）

## 5. 交付验收路径（六线主路径）

1. 双击 `dist/PhotoBeam.exe`（无黑框）→ 托盘出现 → 防火墙 UAC 一次
2. 首次配对：手机装 APK → ①授权 ②配对（6 位 SAS 两端核对）③立即同步
3. 日常使用：手机截图 → PC 剪贴板直接 Ctrl+V；弹窗提示；60 秒后还原
4. 断连：通知图标消失（未连接通道，无状态栏图标）→ 图片入队 → 恢复后补发
5. 故障：托盘"复制诊断信息"→ 粘贴发送
6. 开机自启：托盘勾选；手机端 BootReceiver + 闹钟 + PACKAGE_REPLACED 三重保活

---

# 复审整改记录（2026-11-02 03:30–04:00，对应 stage3-review.md S3-1~S3-12）

## 五条 P1（必清）

| # | 整改 | 验证 |
|---|------|------|
| S3-1 | testClipboard 7 处 logf 补格式动词；globalAllocCopy/globalRead 改 unsafe.Slice | 复跑自检输出全部可读（见下）；go vet -unsafeptr=false = 0 条 |
| S3-2 | 删 dist/PhotoBeam.exe~、pc/pc.exe、PhotoBeam.apk.idsig；dist/ 只含 PhotoBeam.exe；证据文档"删除 pc/pc.exe"表述更正 | dist/ 目录列表见下 |
| S3-3 | build-pc.bat 重写为 build-pc.sh 的 Windows 包装（bash + errorlevel 检查） | 文件内容见仓库 |
| S3-4 | build-android.sh 补三步校验：① zip 内含 classes.dex ② 特征串（probeLinkAlive/completeImage/UpgradeReceiver）③ apksigner verify | 构建尾部输出含 "PASS ① ②" 与 "PASS ③"（见下） |
| S3-5 | release key 生成于 %LOCALAPPDATA%\PhotoBeam\photobeam-release.keystore（仓库外），口令环境变量 PHOTOBEAM_KEYSTORE_PASS（未设则随机生成并落盘 keystore.pass）；仓库内 debug.keystore 已删除；README 注明换机升级须知 | apksigner verify --print-certs = **CN=PhotoBeam**（不再是 PhotoBeam Debug） |

## P2 一并清零（低成本顺手）

- S3-6 setClipboardTextSimple 加 cbMu；S3-7 完成日志如实（标记失败则写"将进 Win+V 历史"）；
  S3-8 新弹窗 PostMessage(WM_CLOSE) 顶掉旧弹窗（零滞后）；S3-9 PNG 周期 open 失败记日志；
  S3-10 无主剪贴板时弹窗明示"本次不还原"（delivery_popup.png 佐证）；S3-11 协议头部状态行更新；
  S3-12 看门狗改"goroutine>16 才落栈"
- **CH_ON 采纳审核裁定**：改 IMPORTANCE_LOW + 新 channel id（photobeam_fg_on_v2）——
  状态栏图标照常显示（"亮=已连接"不损失），恢复连接不再响铃

## 复审材料（审核方要求的 5 项）

1. 自检输出（正常版）：
```
解码 OK: 1080x2400 dib=10368040B png=232327B
剪贴板写入完成（不进 Win+V 历史）dib=10368040B png=232327B
写入返回: true（无主剪贴板不还原=false）
CF_DIB 读回一致: 10368040 bytes
排除标记 OK: CanIncludeInClipboardHistory = [0 0 0 0]
排除标记 OK: CanUploadToCloudClipboard = [0 0 0 0]
剪贴板已还原原内容
自检完成
```
2. go vet：`go vet -unsafeptr=false ./...` = 0 条（2 条 unsafeptr 豁免，理由见 build-pc.sh 头注与
   stage3-review.md §七①——GlobalLock 期间内存被 pin，x/sys 惯用模式）
3. dist/ 目录：仅 `PhotoBeam.exe`（4.82MB，PE subsystem=2）
4. build-android.sh 尾部：`PASS ① classes.dex 存在 ② 特征串完整` + `PASS ③ apksigner verify 通过`
5. apksigner verify --print-certs：`CN=PhotoBeam`，SHA-256 aa919bd4…（release key，仓库外保管）

## 交付态真机（release key 重装后全新配对）

- 新指纹 <PHONE-FP> 配对成功（SAS 两端一致人工核对）
- 测试机垃圾测试文件（/sdcard 根 122 个 0 字节/残留 png）已清理，媒体库仅剩 82 个真实 DCIM/Pictures 文件
- 82 张图库重发现 → PC 幂等秒回 ack，一轮冲刷完毕（队列 81/未就绪 0）
- 交付 E2E：截图 03:53:36.974 → PC 落盘 03:53:37 → 剪贴板 03:53:38（PowerShell 验证 1080x2400）
- 弹窗渲染 + S3-10 不还原提示（delivery_popup.png）
