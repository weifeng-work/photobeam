# PhotoBeam M1 施工状态（截至 2026-11-01 20:05）

## 已验证可用（红米 <PHONE-MODEL> / Android 13 / MIUI V14 真机）
1. ✅ 配对全流程：UDP 发现 → TCP → ECDH → SAS 双端比对（443232/248699/508159/248699 多次一致）→ Ed25519 互签 → 双向 pin 持久化
2. ✅ 断线补发：118 张积压图一次性补发落盘（哈希校验 + .tmp 隔离 + 原子 rename）
3. ✅ 实时同步：一次实测 **1.1 秒**（截图 → PC 落盘）
4. ✅ 重装自愈：APK 重装 → TOFU 重建 pin → 补发（注意：该 TOFU 已在 Stage 1 审阅中被否决，
   已改为"断开+提示用户在 PC 点重新配对"，此路径需在 Stage 2 重测）
5. ✅ 三通道发现全部触发过真实连接；端口漂移（5416→5417）发现应答携带新端口 ✓

## 已修复的缺陷（全部有日志证据）
- OLE 格式 GlobalSize 崩溃、GdiplusShutdown AV、Loop Files 默认不列目录、
  Gui.OnEvent("Click") 无效事件、NetworkOnMainThreadException、prefs.apply() 异步写丢配对、
  MIUI 无 Ed25519/X25519 KeyPairGenerator → BC 裸字节实现、Byte.toUInt 符号扩展、
  帧长校验漏 12B nonce、MIUI 安装拦截 → 关闭 MIUI 优化

## 未解决问题（Stage 2 第一优先）
**新截图的待重试项卡在队列**：MIUI 截图落库后文件写入延迟 > 2.4s（内联重试 3×800ms 不够），
入队为 fileId="" 的待重试项；但 60s 对账的待重试 pass 重读仍失败（原因待查——
同路径的文件 1 分钟后应可读），且 1.5s 快速重试链在第一轮后停止触发（疑似主线程
被 prefs commit 或其他操作阻塞，或 handler 链断裂）。
- 已验证：配对/身份/PC pin 均正常（loadPairedPC fp=<PHONE-FP-PREFIX>...）
- 待查：retry pass 的 openInputStream/sha256Stream 为何对 1 分钟前的截图失败；
  快速重试链为何只跑一轮
- 修复方向：① retry pass 失败时输出异常详情；② 检查主线程阻塞；
  ③ 考虑读取改用 openFileDescriptor 或加大内联重试间隔到 1.5s×4

## Stage 1 审阅待办（MiniMax stage1-review.md 的 7 条 P0 已修 5 条）
- ✅ P0-01 manifest property、✅ P0-02 就绪判定+empty_file、✅ P0-03 重试路径（待验证）、
  ✅ P0-04 batch_size、✅ P0-05 删除 TOFU（PC 降级 pairing 已实现）、✅ P0-07 握手先写后读
- ⏳ P0-06 协议 §3.1 salt 文字勘误（未改文档）
- P1 待办：P1-02 指纹前置（已实现，待验证）、P1-03 保活指引（Stage 2）、
  P1-04 部分媒体授权（Stage 2）、P1-05 防火墙 program= 绑定、P1-06 诊断补全、
  P1-07 fp 匹配、P1-09 PC 识别 pair_reject（已实现）、P1-10 Notifier.dropped（已实现）

## 文件位置
- 协议：docs/protocol-v1.md（v1.2）
- PC：pc/*.go → PhotoBeam.exe（4.4MB）
- 手机：android/ → PhotoBeam.apk（2.1MB）
- 审阅记录：.lam/reviews/*.md（4 轮）


## 更新（2026-11-01 20:15）——Stage 1 审阅修复后的真机状态

### 已修复并验证
- P0-01 manifest PROPERTY_SPECIAL_USE_FGS_SUBTYPE ✓
- P0-05 删除 TOFU（手机端断开+提示；PC 端验签失败降级 pairing+reset:true）✓
- P0-07 PC 握手先写后读 ✓
- P1-09 PC 识别 pair_reject、后台发送路径不发 pair_reject ✓
- P1-01 last_port 语义修正（PC 存自身监听端口）✓
- P1-10 Notifier.dropped 接入 ✓
- P1-02 网络指纹前置判定（已实现，待真机验证跨网段场景）✓
- 重连认证多次通过（握手 SAS/签名/加密全链路稳定）✓

### 新发现的问题（当前阻塞项）
**重连握手成功后，file_meta 帧未到达 PC**：手机端握手完成（auth_ok 已收、
savePairedPC 已执行），随后 sendOne 发送 file_meta，但 PC 端 messageLoop
无任何接收日志，连接以 EOF 结束。PC 端无验签失败/异常日志。
- 排查方向：① PC 的 auth→messageLoop 切换处（break 出认证循环后的状态）；
  ② 手机端 file_meta writeFrame 是否真的发出（加发送前后埋点）；
  ③ MIUI FUSE 对截图文件 openInputStream 的阻塞（sendOne 在 file_meta 后
  openInputStream，若阻塞不影响 file_meta 到达——需确认顺序）；
  ④ 快速重试链与冲刷的并发（单线程执行器应已串行化）
- 注意：此前 118 张批量补发成功走的是完全相同的发送路径——对比当时与现在的
  差异（PC 端 P0-07 握手重排、P0-05 降级逻辑、手机端 allowPairing 参数）是关键线索

### 建议下一步
1. 在 sendOne 握手完成后、file_meta 发送前后各加一条日志；PC messageLoop 入口加一条
2. 重跑一次，按日志定位帧丢失位置
3. 修复后送 MiniMax 复核 Stage 1，然后进入 Stage 2


## 更新（2026-11-01 22:30）——Stage 1 第二轮复核结论

### 好消息
- 编译已通过（kotlinc EXIT=0, go build EXIT=0），装机包与磁盘包 SHA-256 一致——"装旧包"已证伪
- 传输层主干忠实落实协议；118 张补发、实时同步真实有效

### 未完成（MiniMax C-01~C-12 必改清单，见 .lam/reviews/stage1-recheck2.md）
核心遗留：
1. **ContentObserver 从未注册**（SenderService.onCreate 漏调 sync.register()）→ 只有 30s 轮询，
   实测截图延迟 2s/25s/20s 不稳定——这就是"不够跟手"的直接原因（C-07 相关联）
2. **方案 A 未落地**：MediaStoreSync 仍在入队时读文件算哈希 → MIUI 延迟场景卡队列；
   改为入队零 IO + 发送时边算边发（file_id 在发送时确定）
3. PC 端幂等集合无上限（长期运行内存增长）
4. 手机端 last_port 存的是对端端口？——不，MiniMax 更正：Java Socket.getPort() 返回对端端口 ✓
   （上一轮我自己误判），真正的错是 handshake 里写的是 sock.port（对端）✓ 其实对——但
   updateLastConnection 一直没被调用的原因同 1（flush 没跑）——接上 observer 后自然解决
5. 协议 §3.1 session salt 文字（本端在前→发送方视角）与 SAS salt 的 Receiver 视角说明要统一
6. 其余：指纹闸门接线、transient 退避计数、每图一连接改复用、MainActivity 计数陈旧、
   "立即同步"按钮无反馈、batch_id 稳定性、queue.json.bak 清理策略

### 下一会话开工顺序
1. 修 C-07：SenderService.onCreate 补 sync.register()（一行）
2. 方案 A：MediaStoreSync 入队零 IO；Sender 发送时读文件边算哈希边发；
   协议 §5/§7.3 同步为 v1.3
3. 其余 C-01~C-12 逐条
4. 真机验收：连续 3 张截图 ≤5s 落盘 + MIUI 延迟场景
5. MiniMax 复核 → Stage 2（剪贴板管线+弹窗+保活指引+自安装向导）→ Stage 3 → 交付


## Stage 1 签字放行（MiniMax 第五轮，2026-11-02 00:1x）
- E-01/E-02/B-09 逐行核对全部落实 ✓
- 端到端时延 28.1s → **227ms**（ContentObserver 推送生效，不在 30s 节拍）
- 三路读取生效（File 直读 EACCES → fd 150813B ✓）
- 队列 91 项/待发 0/未就绪 0 ✓
- 结转清单 R-1~R-7（不阻塞 Stage 2，M3 前清零）：协议 v1.3 全量同步、build-android.bat、
  assignBatches 冻结、指纹闸门接线、is_pending 过滤/200MB/两次采样/File 直读注释更正、
  通知栏 M 张暂不可读、立即同步按钮、PC seenIDs 上限

## Stage 2 施工图（剪贴板管线，纯 PC 端 Go）
MiniMax 标注的 6 个坑：
1. CF_DIB 非 CF_BITMAP（Go image/jpeg+image/png 解码 → 32bpp BGRX bottom-up 手工构建）
2. 60s 还原前比对剪贴板序号（GetClipboardSequenceNumber）
3. 快照旧内容不碰 delayed rendering（读不到就放弃还原）
4. 文案不承诺 Win+V"彻底消失"（只承诺不进本机历史）
5. 弹窗用 x/sys/windows 自绘无边框置顶窗（WS_EX_NOACTIVATE|TOPMOST|TOOLWINDOW；WinRT toast 需 AUMID 注册否则静默）
6. 落盘改为托盘可开关、默认只进剪贴板；写入在 finalizeFile 后异步触发

### clipboard_windows.go 要点（~200 行）
- RegisterClipboardFormatW("CanIncludeInClipboardHistory"/"CanUploadToCloudClipboard")
- OpenClipboard→EmptyClipboard→SetClipboardData(CF_DIB+CF_PNG→RegisterClipboardFormat("PNG"))→排除格式→CloseClipboard（同一周期）
- GetClipboardSequenceNumber 快照比对
- Go 解码：image/jpeg+image/png → RGBA → 32bpp BGRX bottom-up（DIB）
- heic → 跳过剪贴板只落盘 + 弹窗"已存入文件夹"

### popup_windows.go 要点（~150 行）
- RegisterClassEx + CreateWindowEx(WS_POPUP|WS_EX_NOACTIVATE|WS_EX_TOPMOST|WS_EX_TOOLWINDOW)
- WM_PAINT: FillRect(深色背景) + DrawText(文件名+提示)
- SetTimer 5s → DestroyWindow；WM_LBUTTONDOWN → DestroyWindow
- 位置：MonitorFromPoint + GetSystemMetrics(SM_CXSCREEN/CYSCREEN) 右下角

### 集成点（main.go）
- flushQueue 成功落盘后 → go clipboard.WriteToClipboardAndRestore(destPath, 60s)
- config.json: clipboard_enabled(bool, 默认 true)、clipboard_seconds(60)
- 托盘菜单加"剪贴板同步(勾选)"
