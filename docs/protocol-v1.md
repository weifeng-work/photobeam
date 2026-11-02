# PhotoBeam 协议规范 v1.3（Stage 3 修订版）

> 状态：v1.3（Stage 3 修订版，§3.1 SAS salt 文档勘误）。Stage 0 已四轮签字，Stage 1~3 实现已按本规范交付。
> v1.3 变更：更正 §3.1 SAS salt 的 Receiver 侧描述（v1.2 误写为 receiver 在前，与
> "按发送方视角固定顺序"规则及两端实现矛盾）——语义不变，纯文档勘误 [R-1]。
> v1.2 变更标注：[N-xx] = 复核新 P0，[R-xx] = 复核 P1。本文档是 M1 实现的唯一依据。

## 0. 术语、角色与版本

- **Sender** = 安卓手机（主动推送方）；**Receiver** = Windows PC（被动监听方）。
- 角色在协议消息层可切换（`role` 字段预留）。**设计依据**：Android 17（targetSdk 37）的
  `ACCESS_LOCAL_NETWORK` 只限制**主动发起**连接的一方，被动接受方不受限——预留角色切换
  是该权限落地后的存活通道。v1 只实现 Sender→Receiver，**不实现运行时角色切换**。
- **编译与目标版本（钉死）**[P0-12]：`compileSdk = 37`，`targetSdk = 36`，`minSdk = 31`。
  | 行为 | 与 targetSdk 36 的关系 |
  |---|---|
  | `specialUse` FGS | 可用（API 34+；API 31-33 降级 dataSync 路径见 §7.5） |
  | `SCHEDULE_EXACT_ALARM` | targetSdk ≥ 33 新装默认拒绝 → **不申请、不使用** exact 闹钟（见 §7.5/§7.7） |
  | `ACCESS_LOCAL_NETWORK` | targetSdk 36 隐式授予；manifest 预埋声明，37 时仅需打开申请弹窗 |
  | Android 14 部分媒体授权 | targetSdk ≥ 34 触发"选中的照片" → 必须处理（见 §7.7） |
  | Android 15 BOOT_COMPLETED FGS 禁令 | targetSdk ≥ 35 **触发** → 开机流程 = BootReceiver 起闹钟，闹钟起 FGS（见 §7.6） |
  | Android 16 JobScheduler 配额 | 对 Android 16+ 全部应用生效，与 targetSdk 无关（及时性/不丢失的口径见 §7.5） |
- 端口：**UDP 发现端口固定 5416/udp**（被占 → PC 明确告警拒绝启动，不静默漂移——漂移会让
  手机端固定广播永远收不到应答）；**TCP 传输端口可漂移**（5416→5436 自动探测空闲），实际
  端口随发现应答通告。`devices.json` 记录 `last_port`，随每次 found 更新。[P0-11]
- 本协议的可靠性承诺口径：**"只要双方 App 运行且在同一局域网，图片最终一定到（最坏延迟由
  系统后台调度决定）；传输不依赖长连接，不会出现'掉线后要打开 App 才恢复'。"**
  "及时性"与"不丢失"分开表述（见 §7.5）。

## 1. 密钥与身份模型

每台设备持有两个**长期身份密钥对**（首次运行生成，存本机配置目录）：

| 密钥 | 用途 |
|---|---|
| X25519 身份密钥 | **仅用于**：设备指纹计算；及与对端身份公钥的静态 ECDH（其结果 v1 仅作为设备绑定证明保留，**不参与会话密钥派生**）。认证由 Ed25519 签名独占 |
| X25519 临时密钥（每会话新生成） | **与对端临时公钥**做 ECDH → 会话密钥（前向保密，见 §3.1 [N-01]） |
| Ed25519 身份密钥 | 签名握手转录，证明设备身份 |

- 设备指纹 `fp = hex(SHA-256(ed25519_pub_raw(32B) || x25519_id_pub_raw(32B)))[:16]`
  ——拼接使用**各 32 字节 raw 原文**（非 base64 文本）[R-25]。
- 配对时双方交换并 pin：对端 Ed25519 公钥、X25519 身份公钥、fp、dev_name、last_ip、last_port。
- **所有跨端比较不使用时间戳**；时间仅用于本端日志与展示 [P2-01]。
- 每次重连重新握手生成新 session key → **旧帧重放自动失效**，无需额外反重放机制 [P0-06 备注]。

## 2. 发现（三通道，独立短超时，串行降级）[P0-11][R-03]

**网络指纹前置判定**[N-06][S-01]：配对成功时手机记录 `{SSID, 网关 IP}`（BSSID **不进指纹**——
mesh/多 AP 漫游会变 BSSID，误判会阻断发送）。每次发送前先比对当前网络指纹：
- **SSID 不同** → **立即**给出人话提示（"当前 WiFi『X』与配对时的『Y』不同，请切回"，附 PC
  上次 IP 供核对）并**跳过三通道扫描**（<1 秒出提示，不经 7.5 秒超时链）；
- **SSID 相同或读不到（`<unknown ssid>`）→ 判据降级为"网关 IP 或子网不同"**：不同 → 同样
  立即提示；相同（含 mesh 漫游换 AP、网关未变）→ **照常进入三通道，不得因 BSSID 变化阻断**
  [验收 22]；
- prefix > /24 的漏检兜底 = 指纹一致前提下接受可能漏检 + 允许手机端手工输入 PC IP [R-19]。

| 通道 | 机制 | 超时 | 是否等满 |
|---|---|---|---|
| ① 直连 | TCP 连 `last_ip:last_port` | 1.5s | **否**——TCP connect 有即时失败反馈（RST/拒绝）即提前切通道 |
| ② UDP 广播 | 按**真实子网掩码**推导定向广播地址，逐接口绑定发送；受限广播仅补充；目标 5416/udp | 1.5s | **是**（UDP 无失败信号） |
| ③ 单播扫描 | 本接口 /24（prefix > /24 时取前 254 地址并记录"可能漏检"）逐地址**单播 UDP discover** 至 5416/udp（50 并发、每地址 400ms）——不做 TCP 端口扫描，PC 应答自带真实 TCP 端口 | 6s | **是** |

- 发现帧：`{"pb":1,"type":"discover","dev":"<手机名>","ver":1}`
- PC 应答：`{"pb":1,"type":"found","name":"<电脑名>","port":<实际TCP端口>,"fp":"<指纹>","paired":true|false}`
- **`found` 的 `fp`/`paired` 仅为 UI 提示，不构成任何安全判断**；安全判断一律以 §3.3 签名校验为准 [P1-17]。
- **通道自检由手机侧发起** [P1-16]：每次成功传输后手机重发一次广播 discover 记录通道②结果
  （成功/超时+时间戳）；通道③低频探测（10 分钟一次）。三通道各持 `{last_result, last_ts}` 进诊断。
  "上次成功通道"单字段废弃——它会掩盖其它通道失效。
- 发现应答与广播**必须携带当前实际生效的 TCP 端口**（端口漂移后不通告废端口）[P1-新2]。

## 3. 握手、配对与重连

### 3.1 明文阶段：HELLO 与 MODE [P1-01][N-08]

**明文阶段统一封装 = `[u32 BE len][body]`（HELLO 与 MODE 相同），各上限 4 KiB，超限断链。**
**加密阶段的 §4 帧格式不用于明文阶段。** 明文阶段仅 HELLO 与 MODE 两种帧，`mode` **不在**
§4 加密控制消息列表中 [N-08]。

**发送顺序硬性规定：两端各自写完自己的 HELLO 后立刻读对方 HELLO，任何一端不得等待对方先发。**

```
HELLO = { pb:1, type:"hello", ver:1,
          ed_pub:<b64-std 32B>, x25519_id_pub:<b64-std 32B>, x25519_eph_pub:<b64-std 32B>,
          nonce:<b64-std 16B>, dev_name:<string>, role:"sender"|"receiver" }
```
base64 一律 std 带 padding；字符串 UTF-8。

**transcript（字节级唯一）**[P0-01][N-03]：不哈希 JSON 文本。单条 HELLO 拆为固定字段集，
按字段名字典序，每字段编码 `[len(name,u32 BE)][name UTF-8][len(value,u32 BE)][value_bytes]`：

| 字段名 | value_bytes |
|---|---|
| dev_name | UTF-8 原始字节 |
| ed_pub | 32B raw |
| nonce | 16B raw |
| role | 1 字节（0x00=sender, 0x01=receiver） |
| ver | u32 BE |
| x25519_eph_pub | 32B raw |
| x25519_id_pub | 32B raw |

**两条 HELLO 的合并规则**[N-03]（与身份无关的固定顺序，不依赖 fp 比较）：

```
transcript_hash = SHA-256( [0x01][Sender HELLO 的 7 字段项] || [0x00][Receiver HELLO 的 7 字段项] )
```

**会话密钥**[N-01]：`shared = X25519(本端临时私钥, 对端临时公钥)`
（Sender 算 `X25519(sender_eph_priv, receiver_eph_pub)`，Receiver 算 `X25519(receiver_eph_priv, sender_eph_pub)`
——同一对临时密钥，DH 对称性保证相等）。`session = HKDF-SHA256(shared, salt=sender_nonce ‖ receiver_nonce, info="photobeam-session-v1", 32B)`（**按发送方视角固定顺序**，与 SAS salt 同构——不得使用"本端在前"的直觉写法，否则两端 key 不同，GCM 第一帧即失败）。

**SAS**[P0-02]：`hk = HKDF-SHA256(shared, salt=x25519_eph_pub_self||x25519_eph_pub_peer||nonce_self||nonce_peer, info="photobeam-sas-v1", 4B)`；
`sas_num = uint32-BE(hk) mod 1000000`；`sas = format("%06d", sas_num)`。
salt 段顺序**两端一律按 Sender 视角固定顺序**（sender_eph ‖ receiver_eph ‖ sender_nonce ‖ receiver_nonce），与本端角色无关——Sender 和 Receiver 计算用的字节序完全相同 [v1.3 更正：v1.2 把 Receiver 侧误写为 receiver 在前；DH 对称性下那会使两端 hk 不一致，实测两端实现均为 sender 在前，以实现为准]。

### 3.2 模式判定与 SAS 展示 [P0-04][N-08][R-13][R-14]

1. 双方 HELLO 交换完毕后，**Receiver 立即回一条明文 MODE 帧**（封装同 HELLO，[u32 len][body]）：
   `{"pb":1,"type":"mode","mode":"pairing"|"reconnect","fp":"<receiver指纹>","reset":false,
     "err":null}` —— **Receiver 收到 Sender HELLO 后立即校验 `ver`；不匹配则 MODE 帧携带
   `err:{reason:"version_unsupported", min_ver:1, ver:2}` 并关闭连接**（ver 不匹配的报错帧
   就在明文阶段发出）[S-02][验收 17]。判据 = Sender 的 fp（Receiver 由 HELLO 的
   `ed_pub‖x25519_id_pub` 自行计算）是否在 devices.json。**`reset:true` = 该设备的配对记录
   曾被"重新配对"清除** [R-13]。
2. mode=pairing → **两端同时展示 SAS**（Receiver 向导窗 + Sender 全屏页），手机端出现
   "两端数字一致吗？一致/不一致"。
   - **Sender 已 pin 该 PC 但 mode=pairing**（无论 reset）→ 手机端提示
     "该电脑的配对记录已被重置，将更新信任密钥"，经用户确认后清除旧 pin 再继续 [R-13][R-14]。
   - 用户点"不一致" → **任一端均可发** `{"type":"pair_reject"}`（加密帧，双向可发 [R-22]）
     → 双方立即断开；Receiver 向导显示"用户已拒绝配对"，Sender 显示"两端数字不一致，请确认
     没有中间人，重试"。
3. mode=reconnect → 跳过 SAS，直接进 §3.3。**若 Sender 本地无该 fp 的 pin**（PC 重装/换密钥）
   → Sender 断开并提示"电脑的身份已变化，请在电脑端重新配对"。

### 3.3 配对完成与重连认证（双方互签）[P0-03]

加密通道建立后（§4）第一条加密消息：

- **配对模式**：Sender 发 `PAIR_CONFIRM = {type:"pair_confirm", sas:"123456", sig:<Ed25519_sign(transcript_hash)>, dev_name}`；
  Receiver 验 sas 与签名 → 回 **`PAIR_OK = {type:"pair_ok", sig:<Receiver的Ed25519_sign(transcript_hash)>}`**；
  Sender 校验 Receiver 签名，失败静默断开切通道。双方持久化设备记录
  `{fp, ed_pub, x25519_id_pub, dev_name, last_ip, last_port, paired_at}`。
- **重连模式**：Sender 发 `AUTH = {type:"auth", sig:<Ed25519_sign(transcript_hash)>}`；
  Receiver 校验后回 **`AUTH_OK = {type:"auth_ok", sig:<Receiver签名(transcript_hash)>}`**；
  **双方**校验对端签名，任一失败 = 静默断开切下一通道（IP 被抢占：陌生设备无私钥，
  拿不到数据也注入不了队列）。Sender 发现对端 fp 无 pin → 断开并提示重新配对。

### 3.4 速率限制（两类计数分离）[P1-13][R-21]

- **计数点 = 收到 PAIR_CONFIRM 且校验失败时**（MODE 帧不计）[R-21]：每 IP 每分钟 ≤ 5 次；
  连续失败 5 次 → 该 IP 冷却 **60 秒**，冷却期间手机收到明确 `pairing_cooldown` 拒绝（不静默）；
- 重连认证失败只计数、只进日志，**不触发冷却**；
- 计数器在内存（PC 重启清零，如实写明）；诊断含认证失败次数、最近失败原因、最近成功时间。

## 4. 加密帧格式 [P0-05][P0-06][N-08]

```
[u8 frame_type][u32 BE payload_len][12B nonce][AES-256-GCM ciphertext+tag]

frame_type:  0x01 = CTRL_JSON（控制消息，payload 为 UTF-8 JSON）
             0x02 = FILE_CHUNK（payload 为原始文件字节）
payload_len = 明文长度（密文长度 = payload_len + 16）
AAD         = [u8 frame_type][u32 BE payload_len][12B nonce]
MAX_FRAME_LEN = 1 MiB（明文），超限立即断链并计入错误计数
key = session（32B），每帧独立 12B 随机 nonce；重连换新 session → 旧帧重放自动失效
```

- **CTRL_JSON 消息清单**（`mode` 不在此列，属明文阶段 [N-08]）：`ping`/`pong`（15s 间隔，
  45s 无入站帧判死；心跳只维持连接，不是传输前提）、`bye`、`ack`、`pair_confirm`、`pair_ok`、
  `pair_reject`（**双向可发** [R-22]）、`auth`、`auth_ok`、`file_meta`、`file_end`、`error`。
- **`error` 与 `ACK{status:"error"}` 分工**：`error` 用于**加密阶段的连接级/协议级错误**
  （帧解析失败等；**版本不匹配在明文阶段经 MODE 帧的 `err` 字段报告** [S-02]）；
  **文件级错误一律用 `ACK{status:"error"}`** [R-15]。
- 传输完成 Sender 发 `bye` 并主动关闭连接。

## 5. 文件传输（字段表 + 落盘规则）[N-04][N-05]

### 5.1 消息字段表

| 消息 | 字段（CTRL_JSON payload） |
|---|---|
| `file_meta` | `file_id`(SHA-256 hex 小写)、`name`、`size`、`mtime`、`mime`、`captured_at`、`batch_id`、`is_latest_in_batch`(bool)、`batch_size`(int) |
| `file_end` | `file_id` |
| `ack` | `file_id`、`status`("ok"/"error")、`reason`("ok"/"duplicate"/"ext_rejected"/"disk_full"/"invalid_name"/"io_error")、`err_class`("permanent"/"transient") |
| `error` | `reason`、`ver`、`min_ver`（连接级/协议级错误专用 [R-15]） |
| `file_chunk` | 二进制帧（frame_type=0x02），payload = `raw file bytes`；**所属文件与序号由状态机隐含**（M1 单文件顺序传输，见下） |

- **file_id**[P0-07]：Sender 在**入队时**完整读一遍文件计算 SHA-256 并固化到队列项（二遍读
  成本几十到几百毫秒，已计入 §9 第 2 条时延预算）；传输时第二遍读文件流式发送。
  Receiver 幂等集合存 file_id。**ACK 处理顺序 = 幂等集合 → 磁盘配额 → 落盘** [R-15]。
- 状态机（M1 **禁止多文件交错**）[P1-02]：
  `IDLE → file_meta → RECV(expect_seq=0,1,2…) → file_end → ack`。
  chunk 顺序由状态机保证；FILE_END 后同文件 chunk = 协议违规断链。

### 5.2 接收端落盘规则（M1 必须完整实现）[N-05]

- **文件名消毒**：取 base name、剥路径分隔符（`\` `/`）与控制字符、强制保留原扩展名
  —— 防路径穿越（`name` 来自对端，可含 `..\`）；
- **扩展名白名单**：`jpg/jpeg/png/webp/gif/bmp/heic`；白名单外 → `ACK{error, permanent,
  reason:"ext_rejected"}`；
- **隔离写入**：先写 `Received\.tmp\<file_id>.part`，FILE_END 后校验整文件 SHA-256 =
  file_id → 通过则**原子 rename** 到 `<unixMs_Receiver时钟>_<消毒名>`（冲突加 -1/-2）；
  校验失败删 .part 回 transient；
- 落盘名时间戳用 **Receiver 时钟**；跨设备时间比较禁令 [P2-01]。
- heic 在资源管理器无缩略图 → README/托盘提示说明 [P2-02]。

## 6. 批次语义（三层）[P1-12][R-18]

- **开批**：时间窗聚类——与当前批第一张的 `DATE_ADDED` 间隔 ≤ **3 秒** → 同批；超 3 秒 →
  新批。**不含"同一发现源"条件**（同一组连拍若一部分由 Observer、一部分由对账发现，仍是
  同批）[R-18]。"捕获间隔"统一指 `DATE_ADDED`。
- **批内排序与 is_latest**：排序键 = `(DATE_ADDED, MediaStore _ID)`（_ID 单调递增解决同秒
  并列），最后一张 `is_latest_in_batch=true`。
- **batch_id**：`"<fp前8>-<seq>"`，seq 持久化单调计数（重启继续递增）。
- 三层：① 同批仅 is_latest 触发剪贴板动作（M2）；② Receiver 全局最新指针跨批更新；
  ③ Receiver 800ms–1.5s debounce 合并突发，窗口内剪贴板序号变化 → 立即结算。
- **落盘永远全做，剪贴板只有一个槽位。** 所有时间比较仅在 Sender 侧。

## 7. 手机端（Sender）

### 7.1 捕获与两层账本 [P0-08][N-02][R-08]
`ContentObserver` on MediaStore.Images + FGS 内定时对账 + App 启动对账 + 网络恢复即对账。
- **发现账本**（便宜）：键 = MediaStore `_ID`（值 = DATE_ADDED）。**对账扫描阶段绝不读文件
  内容**，只做 `_ID` 水位与集合比对；**仅对去重后新发现的条目读一次文件**（入队时计算并
  固化 file_id，此成本已计入 §9 第 2 条时延预算）[N-02]；
- **账本清理**[R-08]：仅保留"最近 30 天 + 未发送"的条目，其余在对账后清理，防无限膨胀；
- **幂等账本**（file_id 集合）：Receiver 侧，仅传输层。

### 7.2 对账 checkpoint [P0-09][R-09][P2-b]
**主水位 = `_ID`**（不受时钟影响），**按 `VOLUME_NAME` 分组维护各自的 `last_max_id`**
（Android 10+ 的 `VOLUME_EXTERNAL` 聚合多卷，各卷 `_ID` 空间独立，单一水位会漏卷）；
查询 URI 用 `MediaStore.Images.Media.getContentUri(MediaStore.VOLUME_EXTERNAL)`（API 29+）。
流程：`upper_id = 查询时各卷最大 _ID` → 查 `_id > last_max_id[卷]` 的行 → 逐行比发现账本 →
入队（读一次文件算 file_id）→ **结束后才推进 `last_max_id[卷] = upper_id[卷]`**（处理失败
不推进，下轮重查）。`DATE_ADDED` 仅作展示与批次聚类，不参与水位判定 → **时钟调快/调慢都
不漏图**。前台活跃期对账周期默认 60s（进程存活且非受限时）。

### 7.3 队列 [P0-10][P1-05]
持久化 JSON，项 = `{file_id, uri, name, size, mime, captured_at, batch_id,
is_latest_in_batch, state: pending|sending|sent|dropped, tries, drop_reason}`：
- 持久化 = 写临时文件 → fsync → 原子 rename；保留 `queue.json.bak`；启动解析失败 → .bak 恢复；
  **启动时全部 `sending` → `pending`**；
- 双上限 200 张 / 500MB，超限丢**最旧的 pending**（不碰 sending），通知栏告知丢弃数量；
- URI 失效 → 标 `dropped`（drop_reason 记录），不再重试。

### 7.4 发送循环
队首 → §2 **三通道**解析 → 连接 → 握手（重连模式）→ file_meta/chunks/end → ack → 记账。
失败回队列，指数退避 30s→1m→5m→15m 封顶；permanent → dropped。

### 7.5 前台服务与保活 [P0-13][P1-07][R-11][R-16]
- FGS 类型：`specialUse`（API 34+；**API 31-33 降级 `dataSync`**：Android 15 上 6h/24h
  累计超时 → `onTimeout()` → stopSelf → 30s 后重启；**连续超时 3 次 → 降级 Alarm-only 模式**
  （不启 FGS），通知栏提示授予省电白名单）[R-16]；
  **`startForegroundService` 后必须 5 秒内调用 `startForeground()`**（否则系统抛异常）[R-11]；
- 厂商杀后台**不调用 onTimeout** → 自恢复：`START_STICKY` + onStartCommand 重注册 Observer +
  `AlarmManager.setAndAllowWhileIdle()` 自检心跳（非 exact，不申请 SCHEDULE_EXACT_ALARM；
  Doze 下真实节拍 ≈ 15 分钟，如实写明）；
- "服务被杀/后台受限"状态显式可见：通知栏"已暂停，点此恢复"；
- **息屏三级保障**：① FGS + Observer（前台/近期活跃时秒级）；② setAndAllowWhileIdle 心跳
  （Doze ~15 分钟）；③ 省电白名单 + 厂商自启动引导（分厂商页 + 完成回读，未完成显示"后台
  受限"红标）。**对外口径："最终一定到，息屏时可能迟到 ~15 分钟"。**

### 7.6 开机流程 [P0-12]
targetSdk 36 触发 BOOT_COMPLETED FGS 禁令 → BootReceiver 收到开机广播后**只起
setAndAllowWhileIdle 闹钟**，闹钟触发时经 `startForegroundService` 启动 FGS（合法路径），
并遵守 5 秒 `startForeground()` 约束。

### 7.7 权限清单 [P1-06][P1-08][S-01]
`READ_MEDIA_IMAGES`、`READ_MEDIA_VISUAL_USER_SELECTED`（Android 14+ 三态检测：部分授权
→ 明确提示"仅能访问 N 张"并引导全选）、`POST_NOTIFICATIONS`（运行时申请）、`INTERNET`、
`ACCESS_NETWORK_STATE`、`ACCESS_WIFI_STATE`（读 SSID/网络状态）、
`FOREGROUND_SERVICE`、`FOREGROUND_SERVICE_SPECIAL_USE`（34+）/
`FOREGROUND_SERVICE_DATA_SYNC`（33 降级）、`RECEIVE_BOOT_COMPLETED`、
`REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`、`ACCESS_LOCAL_NETWORK`（预埋）。
**可选增强**：`ACCESS_FINE_LOCATION`（仅让 SSID 提示更精确；**不申请也完整可用**——
读不到 SSID 时指纹自动降级为网关 IP/子网判据，见 §2；不为一个提示申请位置权限）。
**不申请** `SCHEDULE_EXACT_ALARM`。`ACTION_SEND` 分享目标：M2。

### 7.8 网络指纹（存储落点）[P2-c]
配对成功时写入 app 私有 config：`{ssid, gateway_ip, paired_at}`；每次网络切换事件更新；
发送前读取并按 §2 判定。不走队列，不落图库。

## 8. PC 端（Receiver）

- **托盘**：`fyne.io/systray`（定案，不用 walk——cgo/MinGW 冲突）；向导/弹窗用
  `x/sys/windows` 原生 Win32；构建脚本 **`CGO_ENABLED=0`** [P1-10]；
- 菜单 = 打开接收文件夹 / 重新配对 / 复制诊断信息 / 修复防火墙规则 / 开机自启(勾选) / 退出；
- **向导窗**：选保存文件夹（默认 `%LOCALAPPDATA%\PhotoBeam\Received`，OneDrive 重定向检测
  + 警告，配置目录同样避开）→ 显示 SAS → 配对成功；
- **网络自检四件套**（诊断置顶）[P1-11]：对外通告 IP + 所属网卡名（`GetAdaptersAddresses`
  IfType 过滤 + 名称黑名单 Hyper-V/WSL/VMware/Tailscale/WireGuard/Loopback/vEthernet）、
  监听端口、防火墙规则存在性 + **规则路径与当前 exe 路径比对**、网络 profile（公用/专用）；
  另含手机可达 IP、默认网关、认证失败统计 [P1-13]；
- **防火墙**[P1-11]：预置两条规则——TCP 5416-5436 与 UDP 5416，按程序路径绑定、绑定**全部**
  profile；启动自检规则存在 + 路径一致，失效自动重建（UAC，拒绝 → 托盘"修复防火墙规则"
  退路 + 诊断置顶）；发现端口被占 → 明确告警拒绝启动 [P0-11]；
- **常驻**[P1-09][R-17]：**登录自启**（HKCU Run 键，登录时生效——措辞如实）+ **计划任务
  看门狗每 5 分钟拉起，以当前用户身份运行（`/RU <user>` + 交互式 `/IT`，避免 Session 0
  看不到托盘）**（M1 就做）；卸载清理说明写入 README；
- 日志：内部环形 200 条 + **崩溃时写退出码与最后 200 条日志**落盘
  `%LOCALAPPDATA%\PhotoBeam\logs` [R-20]。

## 9. 验收标准（v1.2 修订）[P0-14][N-07][N-06][R-10]

时延类判据统一"连续 20 次采样 ≥18 次满足"；每条记录实测值+设备+ROM+是否息屏。
1. 配对全流程：**两端同时显示 SAS**；点"不一致"（任一端）→ 双方断开，PC 显示"用户已拒绝配对"；
2. 截图 ≤3 秒落盘（20 采样 18 次达标，从按下截图键起算）；
3. **PC 程序退出** 1 小时 → 队列累积（≥30 张）→ PC 程序重启后自动补发，批次关系保持；
4. **`adb shell kill <pid>`**（可杀 FGS 进程，保留闹钟/作业；**`am force-stop` 不可用——清空
   闹钟并置 stopped；`am kill` 对 FGS 常驻进程无效**）→ 心跳拉起服务并补上期间的图；
   另 `am set-inactive <pkg> true` 模拟冻结 → 解除 → 补发；
5. **息屏静置 30 分钟 → 期间截图 3 张 → 解锁后 ≤20 分钟内全部落盘**；
6. 手机连另一 SSID → **网络指纹前置判定 <1 秒**出提示"当前 WiFi『X』与配对时的『Y』不同"；
7. 同秒连拍 3 张 → 3 张共享同一 batch_id、恰 1 张 is_latest_in_batch=true、batch_size=3
   （字段见 §5.1）；
8. 预占 TCP 5416 → PC 漂移 54xx 且 found 携带新端口，配对成功；
9. 预占 UDP 5416 → PC 明确告警拒绝启动（不静默漂移）；
10. 断网导入 250 张 → 队列超限丢最旧 pending，通知栏告知丢弃数量；
11. 同一图重复入队 3 次 → PC 只落盘 1 份；传输中断 → 重传后无半截文件、无重复文件（.tmp
    目录崩溃后可清理/续写）；
12. `.txt`/`.exe` 伪装图片 → `permanent` 拒绝，手机不重试、通知说明原因；
13. **拒绝 UAC** → 托盘"修复防火墙规则"入口 + 诊断置顶；profile 切公用 → 全 profile 绑定
    规则仍可入站；
14. 杀 PC 进程 → 看门狗（当前用户身份）5 分钟内拉起；重启 PC → 登录自启 + 队列补发；
15. 手机时钟**调快 2 小时与调慢 2 小时**各测一次 → 落盘与批次标记均不受影响（_ID 水位
    不受时钟影响）[R-09]；
16. 6 次错误配对 → 第 6 次被拒且有提示；一次网络抖动的重连失败**不**触发冷却；
17. `ver:2` 客户端 → PC 回**明文 MODE 携带 `err{reason:"version_unsupported"}`**（不静默断开）；
22. **同一 SSID 的 mesh/多 AP 漫游（BSSID 变化）→ 照片仍能正常发送，不得因 BSSID 变化而阻断**；
    读不到 SSID 时按网关/子网判据降级 [S-01]；
18. 多网卡（WiFi+有线+VPN 虚拟网卡）→ 诊断"手机可达 IP"落在手机同网段网卡，配对成功；
19. Android 14+ 部分媒体授权：选 3 张 → 提示"仅能访问 3 张"并引导全选（**需一台 Android 14+
    真机**，vivo 日常机满足；红米 13 测不到）[R-10]；
20. **在 Android 14+ 真机上验证 `specialUse` 分支能起 FGS**（红米 13 跑的是 dataSync 降级
    分支，specialUse 分支必须另有设备验证）[R-10]；
21. **MIUI/vivo 真机场景**：从最近任务划掉 App / 开启省电限制后，30 分钟内仍能自动补发；
    若不能，App 已显示"后台受限"红标并给出跳转设置入口——**验收判据是"用户能一眼看出为什么
    没传"** [N-07 附件]。

## 10. 范围与延后项

M1 不做：剪贴板写入/还原与弹窗（M2）、EXIF 方向与降采样（M2，M1 落盘原图）、mDNS（M2 可选
加速器）、静默自更新（M3）、ACTION_SEND（M2）、角色切换的实际实现（字段预留）、
PC 反向拉取（协议字段预留，v1 不实现 [P2-新2]）。
`captured_at`（EXIF 来源）与截图类缺失时的取值规则 → M2 前补 [P2 备注]。
"复制诊断信息"的稳定文本字段序 → M2 定稿（当前为给人看的自由文本）[P2 备注]。
