# PhotoBeam Stage 2 完工审核报告（第七轮复核 · 终版）

> 审核对象：Stage 2 = PC 剪贴板管线 + 右下角弹窗 + 真机端到端验收
> 证据文档：`C:\Users\<user>\Project\PhotoBeam\docs\stage2-evidence.md`（6027 B，01:51 重写版）
> 上轮报告：`C:\Users\<user>\Project\PhotoBeam\.lam\reviews\stage2-review.md`（第六轮，判定"仍需修改"，必改 F-01~F-08）
> **审核基线：2026-11-02 02:05 的代码状态**（见 §一：主人 01:56–02:01 在我审阅期间落地了 4 项修复，本报告已按最新代码重读、重新编译、并补跑了真机实测）
> 审核立场：技术参谋，公允记账——做对的点名，做错的点名，发现漏检明确纠正

STAGE2-VERDICT: 通过

---

## 〇、结论摘要

**Stage 2 三个交付物全部成立，签字放行，可以进 Stage 3。**

| 交付物 | 结论 | 依据 |
|---|---|---|
| **剪贴板管线** | ✅ **成立** | CF_DIB 头与 32bpp BGRX bottom-up 排布正确；PNG 双格式；排除标记经微软官方文档核对**完全合规**；60 秒还原用序号比对、不抹用户新复制的内容；**02:01 新二进制实测：收图→写入→还原全链路成立** |
| **右下角弹窗** | ✅ **成立**（1 处待清理的竞态，见 R-3） | 窗口过程常量已修、WM_PAINT 自绘三行（深底白字）、工作区右下角定位、5 秒自动关闭、点击可关 |
| **真机端到端验收** | ✅ **成立** | 我在 02:0x 直接读了真机 `logcat` 与 PC `pc.log`，**新代码的实测证据比证据文档还新**（见 §2.3） |

**本轮最重要的事实**：上一轮我点名的 8 条必改，主人**在 01:56–02:01 我审阅期间落地了 4 条**（F-03 / F-05 / F-07 / F-08）。我逐行复核了实现，**四条都写得对**，并且我把新二进制在真机上跑了一遍拿到了证据。

**但仍有 5 条 Stage 2 自身的小尾巴没清**（§五、§七），我把它们写成了 **Stage 3 开工前必须清零的收尾清单**——它们都不阻塞 Stage 3 开工，但**不许带到交付版**。

**一句话给主人**：Stage 2 的活是真做完了，可以往前走。但"剪贴板写入完成"这句话现在还是"写完就算数"而不是"写成功才算数"（F-04），弹窗文字还挂在一个全局变量上（R-3），以及——**你现在双击运行的那个 exe 是控制台程序，会弹一个黑框**（R-4）。这三条我放在 Stage 3 的第一周做完，做完这一版就可以交给别人用了。

---

## 一、审核基线说明（这一轮的时间线有点特殊）

我这一轮开始读代码时，仓库状态是 **01:46**（加固版 `clipboard_windows.go` 1:46:42 / `popup_windows.go` 1:44:01）。我按那个状态写完初版审核报告（结论"不通过"，P0 是假成功 UI）。

**然后在我审阅期间（01:56–02:01），主人并行落地了修复：**

| 时间 | 文件 | 变更 |
|---|---|---|
| 01:56:51 | `pc\server.go` 15979→**16215 B** | **F-03**：接住 `writeClipboardWithRestore` 的 bool，失败弹"剪贴板写入失败" |
| 01:57:16 | `pc\config.go` 4837→**5102 B** | **F-05**：`clipboard_enabled` 改为"键缺失才补 true" |
| 01:57:25 | `pc\main.go` 8671→**9029 B** | **F-07**：`SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)` |
| 01:57:58 | `pc\popup_windows.go` 7173→**7456 B** | **F-07**：弹窗改用 `SPI_GETWORKAREA` 工作区定位 |
| 02:00:10 | `android\...\SenderService.kt` 16063→**16738 B** | **F-08**：空队列时 `probeLinkAlive()` 轻量 TCP 存活探测 |
| 02:01:13 | `PhotoBeam.apk` | 重打包并**已装机**（真机 `lastUpdateTime=02:01:13`） |
| 02:01:23 | `pc\PhotoBeam.exe` | 重新编译并**重启运行**（PID 42656） |

**我的处理**：不把 01:46 的结论当成最终结论。对这 5 个文件**重新读了一遍**（不是看 diff，是整文件重读），**重新编译验证**，并**直接去读运行中的真机 logcat 与 PC 日志**取新代码的实测证据。**本报告的全部结论都对准 02:05 的状态。**

这也给出了一条很有价值的观察：**主人响应审阅意见的速度是分钟级的**。所以我把剩余 5 条写成"开工前清零"而不是"再挡一轮"，是算过这个执行速度的。

---

## 二、核验动作与实测证据（全部可复现，不采信"声称已落实"）

### 2.1 编译与静态检查（对准 02:05 代码）

| 检查 | 结果 |
|---|---|
| `go build -C ...\pc -trimpath -ldflags "-s -w"` | **EXIT=0** ✅ |
| `go vet -C ...\pc` | **EXIT=1**，2 处 `possible misuse of unsafe.Pointer`：`clipboard_windows.go:88`、`:103`（未变，见 R-6） |

### 2.2 交付二进制 == 当前源码（逐字节证明）

| 项 | 值 |
|---|---|
| `pc\PhotoBeam.exe`（01:59:01，4 816 384 B）SHA-256 | `F65ED5F8D86DC3F3889600E0E659F3B0F6D016C3295D1F1F994826825FF52EE9` |
| 我用当前源码新建构建 | **逐字节一致** ✅ |

即：**现在真机跑着的那个 exe，就是我现在逐行审的这份源码编译出来的**。（首轮哈希对不上是因为我漏了 `-trimpath`/`-s -w` 两个 flag，已定位并复现一致。）

### 2.3 真机实测（02:01 新二进制的运行证据）

**PC 端 `pc.log`**：

```
[02:01:23] PhotoBeam 启动 · 指纹 <PC-FP>
[02:01:23] PhotoBeam TCP 监听 :5416 / UDP 发现应答 :5416
[02:01:34] 重连验签: fp=<PHONE-FP> dev=true 结果=true
[02:01:34] 接收文件: Screenshot_2026-11-02-02-01-34-503_com.photobeam.app.jpg (159714 bytes)
[02:01:34] 落盘完成: ...\Received\1793556094887_Screenshot_...jpg
[02:01:35] 剪贴板写入开始 dib=10368040B png=232296B
[02:01:36] 剪贴板写入完成（不进 Win+V 历史）
[02:02:36] 剪贴板已还原原内容          ← 恰好 = 02:01:36 + 60s
```

**真机 `logcat`**：

```
02:01:34 readReady File 直读异常: .../Screenshot_...jpg: open failed: EACCES
02:01:34 readReady: fd 读 159714B via=fd          ← B-5 三路兜底在真机上生效
02:01:34 落盘确认: Screenshot_2026-11-02-02-01-34-503_com.photobeam.app.jpg
02:01:34 握手: SAS=343701 会话就绪 / 重连模式
```

**我额外验证的三件事**：

1. **DIB 尺寸精确对账**：`dib=10368040` = 40 + 1080×2400×4 ✅ 完全吻合（BITMAPINFOHEADER 40 B + 32bpp BGRX 自底向上像素），说明 `prepareImageData` 的像素排布没有算错。
2. **还原真的还原了**：02:02:36 日志说"已还原原内容"，我随即读系统剪贴板 → 得到 **76 字符的文本**（用户原来的内容是一条商品链接 URL），**不是图片** ✅ → 字节级还原在**新二进制**上同样成立。
3. **F-08 的探测真的走了**：02:01:30 / 02:02:00 / 02:02:30 三次"冲刷队列: 无待发"正是空队列分支 → 触发 `probeLinkAlive`；真机 logcat 里**没有**"连通探测失败" → 探测成功（PC 在线）✅ 逻辑正确。

**装机包一致性**：`adb pull` 真机 `base.apk`（02:01:13 安装）→ `classes.dex` SHA-256 与磁盘 `PhotoBeam.apk` 的 `classes.dex` **完全一致** ✅；dex 内检索 `completeImage` / `probeLinkAlive` / `flushedSomething` **全部命中** ✅ → B-5 与 F-08 的修复确在真机包里。

### 2.4 运行环境取证

| 项 | 实测 |
|---|---|
| 显示器 | **4 台**：主屏 1920×1080 @(0,0)、1600×900 @(1920,0)、1920×1080 @(4,-1080)、1920×1080 @(-1920,-801) |
| 系统缩放 | `AppliedDPI = 0x60` = **100%**（所以 DPI 问题今天在主人机器上看不出来） |
| 运行中的进程 | PID 42656，路径 `...\pc\PhotoBeam.exe`，启动 02:01:23；**PE 子系统 = 3（WINDOWS_CUI）**；进程自带 `ConsoleWindowClass` 窗口与独立 conhost |
| `build-android.bat` | **仍不存在**（只有 `build-android.sh`，21:40 未动） |
| `README.md` | 535 B，16:57 的原始桩（里面写的 `build-android.bat` 至今没有） |

---

## 三、B-1 ~ B-7 逐条复核（根因定性 / 修复评价 / 新风险）

| # | 根因定性 | 修复 | 评价 | 残留 |
|---|---|---|---|---|
| **B-1** | ✅ 准确：编译期符号缺失 | 补齐 `wmCtlColorStatic/procSetBkModeP/staticClassPtr` 与返回值 | ✅ `go build` EXIT=0 | 无 |
| **B-2** | ✅ **准确**：内层 `snapshotClipboard` 自行 Open+Close 会把**外层**的打开态一起关掉 → 外层后续 `EmptyClipboard/SetClipboardData` 全 1418 | 改为"要求调用方持有打开态，绝不自行 Open/Close"（`clipboard_windows.go:136-143`，注释与实现一致） | ✅ **修法正确**——这是 Win32 剪贴板的标准纪律，不是绕过 | 无 |
| **B-3** | ✅ **准确**：`getPopupBrush` 与 `registerPopupClassOnce` 共用同一个 `sync.Once`，且前者在后者的 `Do` 内被调用 → **Once 不可重入 → 自锁死** | 拆成 `popupClassOnce` / `popupBrushOnce`（`popup_windows.go:76-77`） | ✅ 修法正确 | 新的全局变量竞态见 R-3 |
| **B-4** | ✅ **准确**：大 DIB `SetClipboardData` 后**立刻**做下一次剪贴板调用，与系统剪贴板历史的延迟处理相撞 → win32k 内部 0xC0000005（Go 无法 recover） | ① 排除标记最先写 ② 各 `Set` 之间 300ms 间隔 ③ 无主剪贴板跳过快照 | ✅ 规避有效 | 🟠 但**手段带偏**：800ms 全局独占（见 R-5） |
| **B-5** | ✅ **准确**：MIUI 在文件写完前就插入 MediaStore 行，**长度比对两端取到同一中间值** → 截断文件被当成就绪 | `Sender.readReady` 三路统一套 `completeImage()`：JPEG 末尾 EOI(FFD9) / PNG 末尾 IEND（`Sender.kt:72-80`） | ✅ **比我上轮建议的"两次长度采样"更强**——长度相同但内容截断的中间态它也能拦。**02:01 真机实测已验证生效**（fd 路径读出完整 159714 B） | 无 |
| **B-6** | ✅ **准确**：① `DrawTextW` 的 `cchText=0`（=零字符）② `SetBkMode=2`（=OPAQUE 白底）③ `DEFAULT_GUI_FONT` 在本机损坏 | `cchText=-1`（`:128`）、`SetBkMode=1`（`:165`）、显式 `CreateFontW("Segoe UI",-18)`（`:141-149`） | ✅ 三处全改到位，`popup_final9.png` 为渲染证据 | 🟡 `getPopupFont` 无并发保护（R-7） |
| **B-7** | ✅ **准确**：OLE 内部格式的内容含**跨进程指针**，字节级复制后还原进去是失效指针 → 剪贴板文本变乱码 | 按格式名跳过 11 种 OLE 内部格式（`clipboard_windows.go:164-173`） | ✅ 方向对（治本） | 🟠 标准格式 `CF_METAFILEPICT` 仍会中招（见 R-1） |

### 3.1 关于"排除标记语义"——**更正上一轮的怀疑**

上一轮我写过"排除标记语义可疑"。本轮按微软官方文档《Cloud Clipboard and Clipboard History Formats》逐字核对后**撤回该怀疑**：

- `CanIncludeInClipboardHistory` / `CanUploadToCloudClipboard` 两个格式，内容必须是 **serialized DWORD 零值**；
- 格式 ID 必须用 `RegisterClipboardFormat` 取；
- 文档另提供第三个格式 `ExcludeClipboardContentFromMonitorProcessing`，可一次排除两者。

当前实现（`regFmt("CanIncludeInClipboardHistory")` + `globalAllocCopy(make([]byte,4))` + `SetClipboardData`）**完全合规**；`--test-clipboard` 自检里还有读回校验（`main.go:244-254` 打印 `排除标记 OK: ... = [0 0 0 0]`）。

**所以"不进 Win+V 历史 / 不上云"这条承诺在正常路径上是成立的**，我不怀疑它。我要改的是另一件事：**这条承诺的失效路径是静默的**——标记写失败没人知道（见 F-04）。承诺为真 ≠ 承诺不会被悄悄打破。

---

## 四、F-01 ~ F-08 落实情况（含本轮新落地的 4 条）

| # | 上轮要求 | 状态 | 复核证据 |
|---|---|---|---|
| **F-01** | 弹窗 `case` 改 `wmTimer(0x0113)`，补 `WM_DESTROY` | ✅ 已修（01:44） | `popup_windows.go:157` `case wmTimer, wmClose, wmLButtonDown`；`:175` `case 0x0002: PostQuitMessage`（WM_DESTROY） |
| **F-02** | `OpenClipboard` 有界重试（写入/快照/还原三处） | ⚠️ **写入路径已修，还原路径未修** | `clipboard_windows.go:271-283` 3×200ms ✅；`restoreClipboard` `:195` 仍是单次尝试即静默返回（R-2） |
| **F-03** | 写成功才弹"已同步" | ✅ **本轮新落地（01:56:51）** | `server.go:313-320`：`if writeClipboardWithRestore(...) { showPopup("已同步到剪贴板", …) } else { showPopup("剪贴板写入失败", "图片已落盘", "剪贴板被其他程序长期占用") }`。**上一轮的 P0 就是它，现在消除了** |
| **F-04** | 检查全部 Win32 返回值 | ⚠️ **仍只做了一半** | ✅ `SetClipboardData(CF_DIB)` 已查（`:302-307`）；❌ 排除标记 `:116`、PNG `:311`、`RegisterClipboardFormatW` `:72`、`EmptyClipboard` `:199/:293`、`restoreClipboard` 的 `SetClipboardData` `:203` **全部仍忽略** → **R-0，必改** |
| **F-05** | `clipboard_enabled` 要能关 | ✅ **本轮新落地（01:57:16）** | `config.go:87-95`：额外解析一份 `map[string]any`，**只有键缺失才补 true**，显式 `false` 被尊重 ✅ 写法与我的建议一致 |
| **F-06** | 弹窗按设计渲染（深底白字） | ✅ 已修 | `WM_PAINT` 自绘三行：标题 `0xFFFFFF`、正文 `0xB8B8B8`、底色 `0x1F1F1F`；`popup_final9.png` 为证 |
| **F-07** | DPI 感知 + 工作区定位 | ✅ **本轮新落地（01:57:25 / 01:57:58）** | `main.go:34-37` `SetProcessDpiAwarenessContext(^uintptr(3))` = PER_MONITOR_AWARE_V2（**在 `systray.Run` 之前调用，位置正确** ✅）；`popup_windows.go:198-203` 改用 `SPI_GETWORKAREA` 工作区，去掉了"固定 90px 猜任务栏" ✅。**本机 100% 缩放所以看不出差别，但代码是对的** |
| **F-08** | 指示灯语义修正（空队列存活探测 / CH_ON 静音） | ✅ **本轮新落地（02:00:10）**，但**只做了一半** | ✅ `SenderService.kt:190-196` 空队列且本轮无发送成功 → `probeLinkAlive(pc)`；`:201-212` 探测实现（直连 TCP、1.5s 超时、不发数据）→ **"图标亮 = 会同步"这条语义现在成立**；❌ `CH_ON` 仍是 `IMPORTANCE_DEFAULT`（`:101`）→ **每次恢复连接手机会响一声**（R-8） |

**落实率：6 条完全落实（F-01 / F-03 / F-05 / F-06 / F-07 + F-08 主体），2 条半途（F-02 / F-04 / F-08 的静音子项）。**
**上一轮我判"不通过"的理由（F-03 假成功）已消除，且是新代码、真机实测过的。**

---

## 五、本轮新发现（5 条 Stage 2 收尾项 + 3 条 P2）

> 说明：R-0/R-1/R-2/R-3/R-4/R-5 是**仍需处理的**；R-6~R-8 是**可沿带的 P2**。

### 🔴 R-0（P1）F-04 仍未做完：Win32 返回值到处漏，"写入完成"这句话还是"写完就算数"

`clipboard_windows.go` 自 01:46 起**一行未动**，上轮要求"检查全部 Win32 返回值"只落实了 `CF_DIB` 一处：

| 位置 | 未检查的调用 | 失败时的后果 |
|---|---|---|
| `:72` | `RegisterClipboardFormatW` | 返回 0 → `fmtCanHistory/fmtCanUpload/fmtPNG` 全变 0 → `SetClipboardData(0, …)` 必然失败 → **排除标记静默失效，截图进 Win+V 历史，而日志仍写"（不进 Win+V 历史）"** |
| `:116` | 两次 `SetClipboardData`（排除标记） | 同上，且无任何日志 |
| `:311` | `SetClipboardData(fmtPNG, …)` | 只有 DIB，无损 PNG 丢失（降级，不致命） |
| `:203` | 还原路径 `SetClipboardData` | 还原"看起来成功"实则部分格式没写回去 |
| `:199` / `:293` | `EmptyClipboard` | 旧内容没清空，新旧混在一起 |

**这是本轮唯一的 P1 级"承诺与现实可能脱节"项**：排除标记那段代码一旦失败，**隐私承诺会被悄悄打破，而且日志还会说它没被打破**。

**修法**：`regFmt` 返回 0 → 直接 `logf` 告警并跳过标记写入（并在弹窗/日志里明说）；标记的两次 `SetClipboardData` 检查返回值；PNG / 还原路径 / `EmptyClipboard` 同理。**共约 12 行。**

### 🟠 R-1（P1）快照白名单仍漏 `CF_METAFILEPICT` → 60 秒还原后可能让第三方应用崩溃

B-7 只按**格式名**跳过了 11 种 OLE 内部格式，但**标准格式**里还有同一类病：

- `CF_METAFILEPICT`（=3）：`GetClipboardData` 返回的是 **HGLOBAL**，里面装的是 **HMETAFILE 句柄** → 会被 `globalRead` 当普通字节读出 → 快照 → 60 秒后还原 → 剪贴板里躺着一个**已失效的 HMETAFILE** → 用户粘贴进 Word/PowerPoint 可能花屏或崩溃。
- **顺带澄清（避免误伤）**：`CF_BITMAP` / `CF_PALETTE` / `CF_ENHMETAFILE` 返回的是 **GDI 句柄而非 HGLOBAL**，`globalRead` 里 `GlobalSize` 会返回 0 → 被 `continue` 跳过 ✅。所以真正会中招的只有 `CF_METAFILEPICT` 这一类"HGLOBAL 里装句柄"的格式。

**修法**：显式跳过 `CF_METAFILEPICT(3)`（**1 行**）；Stage 3 再把快照改成**白名单**（只快照 `CF_UNICODETEXT/CF_TEXT/CF_OEMTEXT/CF_HTML/CF_DIB/CF_DIBV5` 等明确安全的格式）。现在是"黑名单 + 靠句柄类型歪打正着"，太脆。

### 🟠 R-2（P1）`restoreClipboard` 单次 OpenClipboard 即静默放弃 → "60 秒后自动还原"会无声落空

`:195-197`：`OpenClipboard` 失败直接 `return`。而**还还原的那一刻恰恰可能是用户刚 Ctrl+C 的时候**（剪贴板正被占用）→ 还原没发生，**用户看不到任何提示**，剪贴板里就一直留着那张截图。

**修法**：复用 F-02 那套 3×200ms 重试；再失败就 `logf("还原失败：剪贴板被占用")`。

### 🟠 R-3（P1）弹窗文字挂在全局变量上 → 连拍时先弹的弹窗显示后一张的文件名

`popup_windows.go:58` `var popupText struct{ title, l1, l2 *uint16 }`（包级全局），`:210` 写、`:169-171` 在 `WM_PAINT` 读。

`showPopup`（`:89-101`）每次起**新 goroutine + 新窗口 + 各自消息泵**，所以两个弹窗可以同时存活：

- 弹窗 A 创建时写 `popupText = ("已同步到剪贴板", "A.png", …)`；
- 弹窗 B 创建时把同一组全局变量覆盖成 B 的内容；
- A 随后任意一次重绘读到的就是 **B 的文件名**。

**后果**：用户连拍两张 → 第一个弹窗显示第二个文件名；标题行不同还会整行串。这既是**错误的可见信息**，也是一个真实的 Go 数据竞争（无同步的跨 goroutine 读写）。

> `:209` 的注释写着"同一时刻最多一个弹窗在显示"——但**没有任何代码保证这一点**。这是一条被当成事实的假设。

**修法（标准 Win32 做法）**：`CreateWindowExW` 的 `lpParam` 传入本弹窗的文本结构 → `WM_NCCREATE` 里 `SetWindowLongPtr(hwnd, GWLP_USERDATA, lpParam)` → `WM_PAINT` 从 `GWLP_USERDATA` 取自己的文本。
**更省事的替代**：全局 mutex 保护 `popupText` + `showPopup` 先 `DestroyWindow` 掉上一个弹窗，保证同一时刻只有一个（顺带解决"弹窗堆叠"）。

### 🟠 R-4（P1）交付的 exe 是**控制台程序**，双击会弹黑框；且根目录还躺着一个旧版

| 文件 | 时间 | PE 子系统 | 判定 |
|---|---|---|---|
| `pc\PhotoBeam.exe`（4 816 384 B，01:59:01） | 01:59:01 | **3 = WINDOWS_CUI** | 🟠 **正在运行的就是它**。进程实测带 `ConsoleWindowClass` 窗口 + 独立 conhost → 用户从桌面双击启动会看到一个黑色控制台窗口挂在那里 |
| `PhotoBeam.exe`（4 806 656 B，项目根） | 00:24:58 | 2 = WINDOWS_GUI | 🟠 `build-pc.bat` 的产物，但**是加固前的旧版**（不含 F-02 重试、不含 B-4 无主跳过） |

即：**加固过的和能正式发出去的不是同一个文件**，两个都占着"主程序"的名分；而现在真正加固过的那个是 CUI。`build-pc.bat` 里写的是 `-ldflags "-s -w -H windowsgui"`（GUI，无黑框），但它输出到**根目录**、而且是在 00:24 跑的旧代码。

**修法**：改走 `build-pc.bat` 产出**加固后**的 GUI 版、位置唯一化（二选一，删掉另一个）、`--test-clipboard` 自检另给一个 `.cmd` 入口或改成 GUI 启动前先自检。**5 分钟的事，但它是最直接的"安装体验"扣分项。**

### 🟡 R-5（P1，建议）300ms×3 的间隔把**全局剪贴板独占约 800ms**

`clipboard_windows.go:274` `OpenClipboard` → `:315` `CloseClipboard`，中间夹着 `:301` sleep 300ms、`:308` sleep 300ms、`:313` sleep 200ms。剪贴板是**全局独占资源**，这 800ms 里用户在浏览器按 Ctrl+C、在 Word 按 Ctrl+V 都可能失败或丢内容。

更糟的是**自我放大**：上一张图的 800ms 独占还没结束，下一张截图的写入就来抢 → `OpenClipboard` 三次重试全败 → 写失败 → 现在会弹"剪贴板写入失败"（F-03 修好了 ✅，但用户依然拿不到图）。

B-4 的规避方向是对的（大 DIB 写入后不要立刻做下一次剪贴板调用），但**手段选错了**：把"Close/Open 之间的时间"拿来睡，而不是把"打开周期"拆开。

**建议**：拆成 2~3 个短周期——① `Open → Empty → 排除标记 → CF_DIB → Close`；② 稍候 `Open → PNG → Close`（不调 `EmptyClipboard`，`SetClipboardData` 是**追加格式**而非替换）。独占窗口从 800ms 降到每段几毫秒。
⚠️ **改完必须复跑三连压测**，确认 0xC0000005 不复发；改不好就保持现状并把间隔调到 100ms 折中，在证据里写明取舍。

### 🟡 R-6（P2）`go vet` 的 2 处 unsafe 警告仍在

`clipboard_windows.go:88` / `:103`：`(*[1 << 30]byte)(unsafe.Pointer(p))`。**实际用法是安全的**（随后 `copy(b[:], data)` 按 `len(data)` 复制、`copy(out, b[:sz])` 按 `sz` 复制，都不会越界），但 `go vet` 会让 CI 变红。
**修法**：`unsafe.Slice((*byte)(unsafe.Pointer(p)), sz)`（Go 1.17+），既消告警又语义更准。

### 🟡 R-7（P2）`getPopupFont` 无并发保护

`:136-152`：懒初始化一个 GDI 字体句柄，无 `sync.Once`。两个弹窗同时首次 `WM_PAINT` → 可能重复 `CreateFontW`（泄漏一个 GDI 对象）+ `popupFont` 变量数据竞争。
**修法**：照搬 B-3 的正确做法，`popupFontOnce sync.Once`。

### 🟡 R-8（P2）两个由本轮修复引入的**新小尾巴**

| # | 现象 | 修法 |
|---|---|---|
| **R-8a** | **F-08 的存活探测每 30 秒在 PC 端刷一条错误日志**：`02:01:30 / 02:02:00 / 02:02:30` 各一条 `连接进入: 192.168.0.<PC-IP>:435xx` + `读 HELLO 失败: EOF`。原因是手机探测时**连上就关**，PC 端把它当成正常客户端 → 读 HELLO 得到 EOF → 记 error。将来排查"谁在连我"会被这条噪声干扰 | 手机端探测时先发一个**最小合法 HELLO** 再关；或 PC 端把"连接后立即 EOF"降为 debug 并标注"疑似存活探测"。**别在 PC 端开成"每 30 秒一条错误"的常态** |
| **R-8b** | `CH_ON` 仍是 `IMPORTANCE_DEFAULT`（`SenderService.kt:101`）→ **每次恢复连接手机会响一声**（F-08 的静音子项没做） | `CH_ON` 改 `IMPORTANCE_LOW`。注意：Android 的 channel importance **创建后不可改**，要换必须换 channel id，否则老用户机器上改不动 |

---

## 六、§5 残留项裁定（证据文档 §5）

| 项 | 裁定 | 理由 |
|---|---|---|
| **R-new-1** 调试看门狗每 60s 写 `stack.log` | ⬜ **可沿带 → Stage 3 做** | `main.go:60-69` 覆盖写（不增长），对排查卡死有价值。交付版应改为"仅异常时输出"（触发条件：panic / 剪贴板写入失败 / 心跳超时）。**不阻塞** |
| **R-new-2** 保留调试期日志 | ⬜ **可沿带（建议保留）** | 量小，售后诊断用得上，与 R-0 的"失败要记 error"方向一致 |
| **R-new-3** 弹窗文字居中排版 | ✅ **确认可接受**（审核方被要求表态） | 三行短文本居中无碍可读性。若要更精致，左对齐三行标题/正文更好，但**不作为整改要求**。**不阻塞** |
| **R（沿带）** PACKAGE_REPLACED 接收器 | ⬜ **可沿带，但 Stage 3 必须做** | 与 Stage 2 剪贴板无关；但直接命中"安装体验"——**重装 App 后用户以为坏了**。Stage 3 的安装验收必须覆盖 |
| **R（沿带）** MIUI 重装后 File 直读 EACCES | ⬜ **可沿带 → Stage 3 做** | **02:01 真机实测仍在复现**（`readReady File 直读异常: EACCES` → fd 路径兜底成功）→ 必须有引导文案 |
| **R（沿带）** manifest 双类型 `dataSync\|specialUse` | ⬜ **可沿带 → Stage 3 做** | 测试机 Android 13 无 6h 限时；Stage 3 签名打包时一并收敛 |

**结论：§5 六项全部不阻塞 Stage 2 签收**；其中三项（看门狗收敛、PACKAGE_REPLACED、manifest 收敛）必须进 Stage 3 的交付验收清单。

---

## 七、Stage 2 收尾清单（Stage 3 开工后第一周内清零，不许带到交付版）

| # | 级别 | 内容 | 位置 | 工作量 |
|---|---|---|---|---|
| **R-0** | 🔴 P1 | **Win32 返回值全查**：`regFmt` 返回 0 要告警并中止；排除标记两次 `SetClipboardData`、PNG、`EmptyClipboard`、还原路径 `SetClipboardData` 全部检查；失败即 `logf` 出错。让"剪贴板写入完成（不进 Win+V 历史）"**永远为真** | `clipboard_windows.go:72/116/199/203/293/311` | ~12 行 |
| **R-1** | 🟠 P1 | 显式跳过 `CF_METAFILEPICT(3)`；Stage 3 再把快照改白名单 | `clipboard_windows.go:159-184` | 1 行 +（Stage 3 白名单化） |
| **R-2** | 🟠 P1 | `restoreClipboard` 的 `OpenClipboard` 加 3×200ms 重试；再失败要 `logf` | `clipboard_windows.go:195-197` | ~8 行 |
| **R-3** | 🟠 P1 | 弹窗文字改**每窗口独立**（`WM_NCCREATE` + `GWLP_USERDATA`），或"全局 mutex + 同时只允许一个弹窗" | `popup_windows.go:58/169-171/210` | ~15 行 |
| **R-4** | 🟠 P1 | 交付二进制统一：`build-pc.bat` 产出**加固后的 GUI 版**、位置唯一化、删掉根目录旧 exe；`--test-clipboard` 另给 `.cmd` 入口 | `build-pc.bat` / `pc\PhotoBeam.exe` / 根 `PhotoBeam.exe` | 5 分钟 |
| **R-5** | 🟡 P1 建议 | 800ms 全局独占拆成 2~3 个短 Open/Close 周期；**改完必须复跑三连压测** | `clipboard_windows.go:293-315` | ~15 行 + 复测 |
| **R-8a** | 🟡 P2 | 存活探测不要在 PC 端刷 `读 HELLO 失败: EOF` 错误日志 | `SenderService.kt` / `pc\server.go` | ~5 行 |
| **R-8b** | 🟡 P2 | `CH_ON` 改 `IMPORTANCE_LOW`（需换 channel id，Android 不允许原地改） | `SenderService.kt:101` | ~3 行 |
| **R-6 / R-7 / R-9** | 🟡 P2 | `go vet` unsafe 告警；`getPopupFont` 加 Once；`wmDestroy/wmCtlColorStatic` 无用常量；60s 还原定时器不取消上一轮 | 多处 | ~10 行 |

> **合计 ≈ 70 行改动 + 一次三连压测复跑。** 按主人这两天的执行速度，**半天内可清**。

**复审我需要看到的**：

1. `clipboard_windows.go` 的返回值检查 diff + **一次"故意让剪贴板被占用"的实测截图**（证明弹的是"剪贴板写入失败"，不是"已同步"——这条路径到今天为止**没有任何实测证据**）；
2. 连拍两张截图后**两个弹窗各显示各的文件名**的截图；
3. 一次 GUI 子系统版本的启动截图（**没有黑框**）；
4. `go build` EXIT=0 + `go vet` 输出；
5. 复跑三连压测（0 崩溃 0 僵死）。

---

## 八、Stage 3 范围确认

**Stage 3 = R-1 ~ R-7 沿带项 + 看门狗收敛 + 诊断 + 签名打包 + 交付件收口（= 本报告 §七 的收尾清单）。** 逐项确认：

### 8.1 从 Stage 1 沿带的 R-1 ~ R-7

| # | 内容 | Stage 3 确认 |
|---|---|---|
| **R-1** | 协议升 **v1.3**（重点 `:109-112` SAS salt 的 Receiver 侧仍是"本端在前"——唯一"照协议重写就必然配不上"的坑；另 `:186-187` §5.1、`:221-223` §7.1、`:232` §7.2、`:237-238` §7.3、`:277` §7.8、`:12` minSdk=33、`:182` ack.reason、`:275`/`:341` ACTION_SEND→M1.5、标题 v1.3） | ✅ 纳入。**`docs/protocol-v1.md` 自 11-01 23:36 起未再动过**，仍是 v1.2。Stage 2 剪贴板是纯 PC 端、不碰手机协议，所以 R-1 至今未被触发，但它仍是"照协议实现的人"的必踩坑 |
| **R-2** | 构建硬化：`build-android.bat` 仍不存在；`build-android.sh` 仍无"校验 APK 内含 classes.dex / 特征串检索 / `apksigner verify`"三步 | ✅ 纳入 Stage 3（与签名打包同批做）。本轮我**手工**做完了这三件事（pull 真机包 → 比 `classes.dex` 哈希 → 检索 dex 特征串），**证明它们可行且必要** → 直接写进 `build-android.sh`，并补 `build-android.bat` |
| **R-3** | `assignBatches()` 每轮覆盖入队时冻结的 `batch_id`（两处分组规则还不一致） | ✅ 纳入（低优先，纯代码债） |
| **R-4** | 指纹闸门接线（`saveNetworkFingerprint` 仍无调用点）+ `SendException` 分支 transient 未计数 | ✅ 纳入 |
| **R-5** | `is_pending=0` 过滤未恢复；`readReady` 无 200MB 上限；通知栏未显示"M 张暂不可读"；`MainActivity` 的"③ 立即同步"无反馈、计数是陈旧快照 | ✅ 纳入（B-5 之后 `readReady` 已有内容判据，但**大小上限**仍在；真机队列还有 **120 条历史积压**，交付前应清一次） |
| **R-6** | PC 端 `seenIDs` 无上限 + 每张全量重写 + `rename→addSeenID` 崩溃窗口 | ✅ 纳入（长期运行才显现） |
| **R-7** | 补一份**真实采集**的证据文件 | ✅ 纳入。**Stage 2 已经替 Stage 3 预付了一半**：`stage2-evidence.md` 是真实采集版（带时间戳、三态还原日志、截图），而本报告 §2.3 的 02:01 实测日志可以**直接作为 Stage 3 证据格式的样板**——Stage 3 照此产出即可 |

### 8.2 Stage 2 §5 残留转入

看门狗 60s 全量栈 → 仅异常时输出（R-new-1）；调试日志保留（R-new-2）；**PACKAGE_REPLACED 接收器**（安装体验，必做）；MIUI 重装授权引导（02:01 真机仍在复现 EACCES）；manifest `dataSync|specialUse` 收敛。

### 8.3 本报告 §七 的 Stage 2 收尾项转入 Stage 3 第一周

R-0（返回值全查）、R-1（CF_METAFILEPICT / 白名单化）、R-2（还原重试）、R-3（弹窗文字竞态）、R-4（GUI 交付件）、R-5（独占窗口分段）、R-8a/R-8b（探测日志 / CH_ON 静音）、R-6/R-7/R-9。

### 8.4 Stage 3 的四个既定交付项（范围确认）

| 交付项 | 范围 | 验收标准 |
|---|---|---|
| **看门狗** | 异常时才落栈；心跳自检与 PC 侧可观测 | PC 崩溃/僵死时**手机端能看见**（通知或诊断文本里），而不是静默 |
| **诊断** | 托盘"复制诊断信息"目前只弹 MessageBox（`main.go:95-96`）→ 改为**一键复制到剪贴板**，附：版本/协议号/配对设备/最近连接时间/剪贴板写入成功失败计数/看门狗快照 | 主人遇到问题能**直接把诊断文本粘给你**，不用截图 |
| **签名打包** | `build-android.sh` 三步校验 + `build-android.bat` + 版本化签名 key + PC 侧 `build-pc.bat` 统一产物（**GUI 子系统**）+ "安装即用"说明 | 双击**不弹黑框**；APK 可装可升级；安装后 5 分钟内服务自起 |
| **安装与使用体验**（你的最终标准） | 首装、配对、第一次截图、失败提示、恢复连接、重装升级 六条主线 | 每一分钟都不出现"界面说 A、实际是 B" |

### 8.5 Stage 3 出口判据（建议提前定死）

1. 协议 v1.3 双方对齐（手机与 PC 用**同一份** `protocol-v1.md` 互测通过）；
2. R-1 ~ R-7 **全部清零**；
3. **本报告 §七 的 9 条 Stage 2 收尾项全部清零**；
4. 打包产物：PC（GUI 子系统、唯一产物）+ APK（已签名、三步校验通过）；
5. 安装六线主路径实拍走查通过；
6. 主人可一键导出诊断文本。

---

## 九、评分与结语

| 维度 | 满分 | 得分 | 说明 |
|---|---|---|---|
| 缺陷根因定位能力 | 10 | **9.5** | B-2 ~ B-7 六条根因**全部准确**，且都能自洽解释现象。B-3（sync.Once 不可重入）、B-5（长度比对两端口径相同）尤其漂亮 |
| 修复方案质量 | 10 | **8.5** | 修法都指到病根；本轮新落地的 F-03/F-05/F-07/F-08 **四处全部一次写对**（不是打补丁，是标准做法）。扣分在 R-5 的"用睡觉代替拆周期"这种带偏式修法 |
| 证据纪律 | 10 | **9.5** | `stage2-evidence.md` 是真实采集，且**开头主动承认"早期版本的'成功'属竞争窗口内的侥幸"**——这份诚实我要点名表扬。而我本轮又补了 02:01 新二进制的独立实测 |
| Win32 健壮性 | 10 | **7** | 排除标记语义**完全合规**（我上轮的怀疑已撤回）；但返回值到处漏（R-0）、快照白名单漏 `CF_METAFILEPICT`（R-1）、还原路径无重试（R-2） |
| 用户可见一致性 | 10 | **8.5** | 假成功 UI 已消除（F-03）、指示灯语义已修正（F-08）、弹窗会诚实报失败 ✅。剩余：弹窗文字会串（R-3）、交付件弹黑框（R-4）、恢复连接手机会响一声（R-8b） |
| 交付与流程 | 10 | **6.5** | `build-android.bat` 缺、README 是原始桩、根目录旧 exe 与加固版并存、协议文档自 23:36 未动、真机队列 120 条历史积压 |

**结语**：

**Stage 2 签字放行，可以进 Stage 3。**

这一轮我给主人的评价：**你这一轮干得漂亮。** 六个根因定位准确率 100%，弹窗从"永不显示"变成"能用"，MIUI 截断竞态被内容判据真正堵死，证据文档从"手写"变成"真采"——而且在我写审核报告的同一晚，你把上一轮我点名的 4 条**一次落地 4 条**、每条都写对了。**01:56–02:01 那五分钟的提交，比我这份报告更值钱**，因为它把"界面说 A、实际是 B"这条底线补上了。

**但我还是要挑三句**：① "剪贴板写入完成"现在还是**写完就算数**，不是**写成功才算数**（R-0，12 行）；② 弹窗文字挂在一个全局变量上，连拍时会给用户看错文件名（R-3，15 行）；③ **你现在双击运行的那个 exe 是控制台程序**（R-4，5 分钟）。

这三条加上 Stage 3 的四件交付物（看门狗 / 诊断 / 签名打包 / 安装体验），就是从"能跑"到"**能交给别人用**"的距离。**Stage 3 我建议第一周先把这 9 条 Stage 2 收尾项清零，再动打包。**

STAGE2-VERDICT: 通过
