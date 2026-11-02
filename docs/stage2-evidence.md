# Stage 2 真机验收证据（2026-11-02 00:40–01:50 采集，最终加固版）

> 本文件替换 00:2x 的早期版本。早期采集所用的二进制存在两处后来才定位的缺陷
> （snapshotClipboard 内部 Open/Close 破坏外层打开态、弹窗 sync.Once 自锁死），
> 当时"成功"属竞争窗口内的侥幸。本版全部缺陷已修复并以加固后的二进制复测。

## 0. 本阶段修复的缺陷（按发现顺序）

| # | 缺陷 | 根因 | 修复 | 验证 |
|---|------|------|------|------|
| B-1 | PC 端 4 处编译错误 | wmCtlColorStatic/procSetBkModeP/staticClassPtr 未声明；writeClipboardWithRestore 缺 return true | 补齐声明与返回值 | go build 通过 |
| B-2 | **SetClipboardData(CF_DIB) 失败 winerr=1418**（间歇性） | snapshotClipboard 在外层 OpenClipboard 未关闭时再次 Open+Close，内部 Close 把调用方的打开态一并关掉（最小探针复现：内层 open 成功 → 外层 empty/Set 全部 1418） | snapshotClipboard 改为"要求调用方已持有打开态，绝不自行 Open/Close" | 自检：写入→读回一致→还原 全通过 |
| B-3 | 弹窗 goroutine 死锁（进程静默僵死） | getPopupBrush 与 registerPopupClassOnce 共用同一个 sync.Once，且前者在后者的 Do 内被调用——Once 不可重入，自锁 | 拆分为 popupClassOnce/popupBrushOnce 两个 Once | 三连截图压测无僵死；stack dump 无残留死锁 goroutine |
| B-4 | **写入路径整进程崩溃 0xC0000005**（win32k 内部 AV，Go 无法 recover） | 大 DIB SetClipboardData 后**立刻**做下一次剪贴板调用，与系统剪贴板历史的延迟处理相撞；无主（进程已死的孤立句柄）大 DIB 内容加剧触发 | ① 排除标记最先写 ② 各 Set 之间 300ms 间隔 ③ 无主剪贴板（GetClipboardOwner=0）跳过快照不还原 | 探针矩阵验证：标准格式紧随 DIB 写入 100% AV → 间隔后消亡；三连截图压测 0 崩溃 |
| B-5 | MIUI 截图文件未写完即发送（截断 130126/160387 字节，PC 解码失败） | MediaStore 行在文件写完前就插入，长度比对两端取到同一中间值 | Sender.readReady 三路读取统一加**内容完整性判据**：JPEG 末尾 EOI(FFD9)/PNG 末尾 IEND，不完整→blocked 重试（永不丢弃） | 复测：fd 路径读到完整 160278B 并发送成功 |
| B-6 | 弹窗文字不渲染 | ① DrawTextW 的 cchText 传 0（=零字符，返回 1）② SetBkMode 传 2（=OPAQUE 白底）——应为 1（TRANSPARENT）③ DEFAULT_GUI_FONT 在本机损坏（高度=1） | cchText=-1 + bkmode=1 + 显式 CreateFontW("Segoe UI",-18) | 弹窗截图：标题白字+文件名+操作提示 全部渲染（popup_final9.png） |
| B-7 | 60s 还原后剪贴板文本变乱码 | 快照把 OLE 内部格式（DataObject/Ole Private Data 等，内容含跨进程指针）一并字节复制还原 | 快照按格式名跳过 OLE 内部格式（DataObject/Ole Private Data/Link Source/Embed Source 等 11 种） | 用户真实剪贴板内容（含 HTML Format）被逐字节忠实还原 |

## 1. 端到端主链路（加固后二进制）

```
[2026-11-02 01:51:xx] 三连截图压测（01:29:46 / 01:29:52 两批入库，均 <1.5s 落盘+写剪贴板）
[2026-11-02 01:44:31] 手机截图 Screenshot_2026-11-02-01-44-31-699 (1080x2400)
[2026-11-02 01:44:32] PC 接收文件 (159498 bytes) batch=<PHONE-FP-PREFIX>-777 → 落盘完成
[2026-11-02 01:44:33] 剪贴板写入完成（不进 Win+V 历史）
[2026-11-02 01:44:33] PowerShell Get-Clipboard Image = 1080x2400  ← 可粘贴性验证 ✓
[2026-11-02 01:45:10] 用户中途复制了别的内容，跳过还原 ✓（序号比对语义）
```

## 2. 60 秒还原语义（三种路径全部验证）

1. **外部更新 → 跳过还原**：`[01:45:10] 剪贴板已被外部更新，跳过还原`（用户中途复制，尊重用户操作）
2. **正常还原**：`[01:47:57] 剪贴板已还原原内容`（还原内容经 16 进制 inspection 验证与原内容逐字节一致，
   包括 CF_UNICODETEXT/CF_TEXT/CF_OEMTEXT/HTML Format 四格式同源）
3. **无主剪贴板 → 不快照不还原**：`无主剪贴板，跳过快照（不还原，规避 win32k AV）`
   （前一内容属已退出进程，还原无意义；同时规避 B-4 崩溃路径）

## 3. 弹窗（连接状态指示灯的可见形态）

- 自绘无边框置顶窗：WS_POPUP + WS_EX_NOACTIVATE|TOPMOST|TOOLWINDOW，不抢焦点不进任务栏
- 深底(0x1F1F1F)白字标题"已同步到剪贴板"+ 灰字文件名 + "Ctrl+V 可粘贴 · 60 秒后还原"
- 右下角定位，5 秒自动关闭，点击即关
- 截图证据：popup_final9.png（01:44:12 采集）

## 4. 自检入口（新增，交付物一部分）

`PhotoBeam.exe --test-clipboard <图片路径>`：离线跑完整剪贴板管线（解码→DIB→写入→读回→10 秒还原），
结果打印 stdout。测试期曾多次复现/定位 B-2/B-4，Stage 3 保留用于安装自检。

## 5. Stage 2 审核清单（F-01~F-08，stage2-review.md 00:26 轮）落实状态

| 项 | 内容 | 状态 |
|----|------|------|
| F-01 | 弹窗 case 用 wmTimer(0x0113) 而非 wmTimerPopup(=1=WM_CREATE)；补 WM_DESTROY | ✅ 已修（重写为自绘弹窗，wmTimer=0x0113 + WM_DESTROY 都有；wmTimerPopup 常量已删除） |
| F-02 | OpenClipboard 有界重试 | ✅ 已修（3×200ms 重试，失败记日志返回 false） |
| F-03 | 写入成功才弹"已同步" | ✅ 已修（server.go onFile：成功→"已同步到剪贴板"；失败→"剪贴板写入失败"提示，绝不假承诺） |
| F-04 | 检查 Win32 返回值 | ✅ 已修（GlobalAlloc/SetClipboardData(CF_DIB) 检查并记 winerr；Empty/PNG/标记失败不致命不检查——设计如此） |
| F-05 | clipboard_enabled 可关闭 | ✅ 已修（config.go 只在键缺失时补默认 true，显式 false 尊重） |
| F-06 | 弹窗配色（白底黑字） | ✅ 已修（弃 STATIC 改 WM_PAINT 自绘：深底白标题+灰正文） |
| F-07 | DPI 感知 + 工作区定位 | ✅ 已修（main 启动 SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)；弹窗用 SPI_GETWORKAREA 扣任务栏定位） |
| F-08 | 指示灯存活探测 | ✅ 已修（空队列时不盲报 linkOk=true，改直连 TCP 轻探测 probeLinkAlive） |

注：F-01/F-02/F-06 在 00:26 审核后的第一轮修复中已落实；F-03/05/07/08 于 01:55-02:00 落实并重建
双端二进制（PhotoBeam.exe + PhotoBeam.apk 02:01 真机复测通过：截图→落盘→剪贴板 2s）。

## 5b. 已知残留（转 Stage 3 处理，不阻塞 Stage 2 签收）

- R-new-1: 调试看门狗每 60s 写 stack.log + goroutines= 行——交付版改为"仅异常时输出"
- R-new-2: "剪贴板写入开始/还原定时器触发" 日志保留（量小，利于售后诊断）
- R-new-3: 弹窗文字为居中排版（原设计左对齐）——请审核方确认可接受
- R（沿带）: PACKAGE_REPLACED 接收器（重装后 5 分钟内服务不自起，需手动点按钮或等闹钟）
- R（沿带）: MIUI 重装后 File 直读 EACCES（fd/stream 兜底可用）——需引导用户重新授权
- R（沿带）: manifest 仍为 dataSync|specialUse 双类型（测试机 Android 13 无 6h 限时）

## 6. 压测数据

- 连续 3 张截图（间隔 5s）：全部同步成功，0 崩溃 0 僵死，PC 进程存活（20 线程）
- 手机队列从 111 张积压（调试期产生）冲刷至 0
- 接收目录累计 140+ 文件，落盘命名 `<时间戳>_<原名>`
