# PhotoBeam 协议规范 v1.6（Stage 6 修订版 · 端到端时延收紧）

> 状态：v1.5（Stage 5 修订版）。Stage 0 已四轮签字，Stage 1~4 实现已按规范交付；
> **v1.4 的修订原则：以实机验证结果为准**——凡 v1.3 的条文与真机行为冲突，
> 一律**改协议而不是回退已验证有效的实现**，每处偏离在 §11 登记（条款 / 实测 / 理由 / 证据）。
>
> **v1.5 变更（Stage 5 第二轮：用户实机反馈驱动）**：
> 1. **§3.1 HELLO 增补 `pair_intent`**（Sender 侧字段）：显式声明本次连接是否由**用户主动发起配对**。
>    Receiver 只在 `pair_intent=true` 时展示配对向导窗；后台自动连接（`false`）不弹窗。
>    字段缺省按 `true` 处理（旧版本行为不变）。**不进入 transcript**，密码学中性。
>    详见 §11 偏离 13。
> 2. **§3.2 向导窗单实例**：Receiver 同一时刻至多一个配对向导窗；新会话必须等旧窗口**确实销毁**
>    后才开新的，「关闭请求早于窗口创建」的竞态必须显式处理。详见 §11 偏离 14。
> 3. **§3.2 配对确认 fail closed**：Receiver 未展示向导窗时收到 `pair_confirm` 一律拒绝配对。
>    详见 §11 偏离 15。
>
> v1.4 变更：
> 1. **§3.2 / §8 / §9.1 SAS 展示落为可实现条款**：Receiver 端由「只落日志」改为**必须弹出向导窗**
>    显示 6 位码并提供「一致 / 不一致」按钮（Stage 5 实测发现原实现只写日志，用户无从核对）。
> 2. **§8 诊断信息不再写系统剪贴板**，改为导出到 `logs\diagnostics-<时间戳>.txt`（Stage 5 决策：
>    复制诊断会 `EmptyClipboard` 且不还原，直接丢弃用户原剪贴板内容）。
> 3. **§0 / §7.7 精确闹钟条款按 MIUI 实测改写**：本实现使用 `USE_EXACT_ALARM`（API 33+ 安装即授予）
>    + `setExactAndAllowWhileIdle`，v1.3「不申请、不使用 exact 闹钟」仅适用于 AOSP 假设。
> 4. **§7.6 开机流程按实测改写**：广播内**直接起服务** + 精确闹钟兜底（v1.3 写的是「只起闹钟」）。
> 5. **§8 计划任务看门狗移出本版范围**（Stage 5 决策：不在系统注册计划任务），登记为后续项。
> 6. §7.7 Android 14 部分媒体授权三态、§7.5 FGS `onTimeout` 降级链路：条款保留，实现状态标注。
>
> **v1.6 变更要点**：§9 第 2 条端到端时延判据由「截图 ≤3 秒落盘」收紧为
> **「截图 ≤2 秒可粘贴」**，并给出两级守门人（CI 可跑的关键路径预算 + 真机端到端脚本）；
> §10 新增两条范围扩张的延后项与代价分析（目录保留 / 任意文件类型）。
>
> v1.3 变更标注：[N-xx] = 复核新 P0，[R-xx] = 复核 P1。本文档是 M1 实现的唯一依据。

## 0. 术语、角色与版本

- **Sender** = 安卓手机（主动推送方）；**Receiver** = Windows PC（被动监听方）。
- 角色在协议消息层可切换（`role` 字段预留）。**设计依据**：Android 17（targetSdk 37）的
  `ACCESS_LOCAL_NETWORK` 只限制**主动发起**连接的一方，被动接受方不受限——预留角色切换
  是该权限落地后的存活通道。v1 只实现 Sender→Receiver，**不实现运行时角色切换**。
- **编译与目标版本（钉死）**[P0-12]：`compileSdk = 37`，`targetSdk = 36`，`minSdk = 33`。
  | 行为 | 与 targetSdk 36 的关系 |
  |---|---|
  | `specialUse` FGS | 可用（API 34+；API 33 走 `dataSync` 分支——**minSdk 33 起 API 31~32 的 dataSync 降级分支不适用**） |
  | `USE_EXACT_ALARM` | **v1.4 按实测改写**：本实现**使用** `USE_EXACT_ALARM`（API 33+ 安装即授予）+ `setExactAndAllowWhileIdle`；v1.3 的「targetSdk ≥ 33 不申请、不使用 exact 闹钟」是 AOSP 假设，**在 MIUI 上与实测冲突**（见 §11 偏离 3） |
  | `ACCESS_LOCAL_NETWORK` | targetSdk 36 隐式授予；manifest 预埋声明，37 时仅需打开申请弹窗 |
  | Android 14 部分媒体授权 | targetSdk ≥ 34 触发"选中的照片" → **必须处理**（见 §7.7）；Stage 5 已实现三态检测与引导 |
  | Android 15 BOOT_COMPLETED FGS 禁令 | targetSdk ≥ 35 **触发** → 开机流程见 §7.6（**v1.4 按实测改写**，见 §11 偏离 4） |
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
- PC 应答：`{"pb":1,"type":"found","name":"<电脑名>","port":<实际TCP端口>,"fp":"<指纹>","paired":true|false,"pair_ready":true|false}`
- **`found` 的 `fp`/`paired` 仅为 UI 提示，不构成任何安全判断**；安全判断一律以 §3.3 签名校验为准 [P1-17]。
- **v1.5 增补（Stage 5 第四轮实测）· 应答必须按指纹甄别**：同一局域网里可能**同时有多台 PhotoBeam PC**
  （同事的、另一台机器上的）。已配对手机（已知 PC 指纹）收到应答时：
  - 应答来自**别的 IP** 且**指纹不同** → 是**别人的机器** → **忽略这条应答，继续等自己那台的**；
  - 应答来自**别的 IP** 但**指纹相同** → 同一台 PC 换了 IP → 算命中；
  - 指纹相同 → 直接命中（不等剩余应答）。
  **首次配对（尚无指纹可比）时**，取首个应答为准 —— 此时没有可比对的指纹，真伪由 §3.3 握手签名把关
  （这也是「局域网里有多台 PC 时首次配对要靠人眼核对 6 位码」的根本原因）。
- **v1.5 增补 · 手工输入电脑 IP 的兜底（R-19 落地）**：手机端必须提供**输入框**，用户填了 IP 时
  **优先走通道①直连**。用于广播被挡（AP 隔离 / 不同 VLAN / 跨网段）的场景 ——
  三通道全失败时「让用户手动输入 IP」必须**真的有地方能填**。
- **通道自检由手机侧发起** [P1-16]：每次成功传输后手机重发一次广播 discover 记录通道②结果
  （成功/超时+时间戳）；通道③低频探测（10 分钟一次）。三通道各持 `{last_result, last_ts}` 进诊断。
  "上次成功通道"单字段废弃——它会掩盖其它通道失效。
- 发现应答与广播**必须携带当前实际生效的 TCP 端口**（端口漂移后不通告废端口）[P1-新2]。
- **v1.5 增补（Stage 5 第五轮）· 配对必须「双方同意」**：
  同一局域网里可能同时跑着**多对** PhotoBeam 用户。手机**首次**配对时手里没有可比对的指纹
  （指纹要握手之后才有），若沿用「取第一个应答就连接」，可能连到**别人的电脑** →
  之后照片持续串到对方机器；反过来，任何人的手机点 ② 都会让**你的电脑**弹窗，
  你若点「不一致」还会**打断对方的配对**。
  因此新增「待命闸门」：
  - Receiver 只有在**待命态**下才受理**新设备**的配对请求。待命态的开启方式：
    用户在托盘点「**配对新设备**」（一次性消耗：成功接上一台即退出）；
    或首次启动且尚未配对过任何设备时**自动待命**（10 分钟）。
  - 待命态通过 `found` 的 **`pair_ready`** 字段广播；Sender 据此在多台候选里
    **优先选「正在等你」的那台**（缺省按 false 处理 = 保守）。
  - **不在待命态**时 Receiver **不得弹窗**，MODE 帧必须携带
    `err:{reason:"receiver_not_ready"}`，Sender 据此**立刻**提示用户
    「让对方在托盘点『配对新设备』」，**不进入 SAS 阶段**（§3.2）。
  - **已配对设备的重连（mode=reconnect）完全不受待命态影响。**
  - Sender 侧必须在 SAS 核对界面显示**对端电脑名**（来自对端 HELLO 的 `dev_name`）——
    用户据此判断「对面那台是不是我的电脑」，这是多用户环境下的唯一可靠锚点。

## 3. 握手、配对与重连

### 3.1 明文阶段：HELLO 与 MODE [P1-01][N-08]

**明文阶段统一封装 = `[u32 BE len][body]`（HELLO 与 MODE 相同），各上限 4 KiB，超限断链。**
**加密阶段的 §4 帧格式不用于明文阶段。** 明文阶段仅 HELLO 与 MODE 两种帧，`mode` **不在**
§4 加密控制消息列表中 [N-08]。

**发送顺序硬性规定：两端各自写完自己的 HELLO 后立刻读对方 HELLO，任何一端不得等待对方先发。**

```
HELLO = { pb:1, type:"hello", ver:1,
          ed_pub:<b64-std 32B>, x25519_id_pub:<b64-std 32B>, x25519_eph_pub:<b64-std 32B>,
          nonce:<b64-std 16B>, dev_name:<string>, role:"sender"|"receiver",
          pair_intent:<bool> }
```
base64 一律 std 带 padding；字符串 UTF-8。

**`pair_intent`（v1.5 增补，Sender 侧字段）**：本次连接是否由**用户主动发起配对**。

- `true` —— 交互路径（手机端用户点「② 配对电脑」）→ **Receiver 必须展示配对向导窗**，
  否则用户无从做人眼核对 6 位 SAS。
- `false` —— 后台自动连接/重连（手机端 SenderService 每 30 秒一次）→ **Receiver 不得展示向导窗**。
  理由：后台连接从不进入人眼核对环节，若照样弹窗，用户正在核对的窗口会被每 30 秒顶掉一次
  （Stage 5 实测：19:11–19:16 每 30 秒弹出一个新向导）。
- **字段缺省 → 按 `true` 处理**（保守：宁可多弹窗，不可跳过人眼核对）。旧版本 Sender 因此行为不变。

**该字段不进入 transcript**：§3.1 的字段表与 transcript 字节序列均未增补，密码学中性。

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
   - **v1.4 落地要求**：Receiver 端**必须弹出向导窗**显示 6 位码（本机指纹 + 设备名 + 码 + 两个
     按钮），**只写日志视为未实现**——用户必须能在 PC 上看到那 6 位数字，否则 Sender 端的
     "请在电脑上核对数字"对话框无法完成，人眼比对退化为无条件信任（Stage 5 实测发现此缺口）。
   - **Sender 已 pin 该 PC 但 mode=pairing**（无论 reset）→ 手机端提示
     "该电脑的配对记录已被重置，将更新信任密钥"，经用户确认后清除旧 pin 再继续 [R-13][R-14]。
   - 用户点"不一致" → **任一端均可发** `{"type":"pair_reject"}`（加密帧，双向可发 [R-22]）
     → 双方立即断开；Receiver 向导显示"用户已拒绝配对"，Sender 显示"两端数字不一致，请确认
     没有中间人，重试"。
   - **v1.4 落地要求**：Receiver 端的"不一致"必须**真的发出** `pair_reject`（v1.3 条款允许双向发，
     但实现只收不发）。
   - **v1.4 新增（Stage 5 实测确立）· 核对窗口 60 秒**：两端各自的 SAS 核对等待上限均为
     **60 秒**，必须成对实现——Receiver 向导窗的定时器与 Sender 对话框的等待是**同一个窗口**，
     任一端先超时都会导致配对失败。**15 秒不够用**：人要读 6 位数字、再看一眼手机、再去点按钮，
     15 秒很容易来不及（实测首轮验收时手机端因等待超时直接发出了 `pair_reject`，
     PC 端日志只留下「手机端已拒绝配对」，用户完全不知道发生了什么）。
     超时后 Sender 端必须**关闭核对对话框**（不能把失效的框留在屏幕上）。
   - **v1.4 新增（Stage 5 实测确立）· 失败路径必须可见**：配对失败（任一端拒绝 / 超时）时，
     Receiver 端必须给出**对话框**提示，不得只发托盘气泡（托盘气泡在用户不盯着托盘时等于没有）。
   - **v1.5 新增（Stage 5 实测确立）· 展示条件 = Sender 的 `pair_intent`（§3.1）**：
     Receiver **只在 `pair_intent=true` 时展示向导窗**；`pair_intent=false`（后台自动连接）时
     只回 MODE=pairing，不展示向导窗——后台连接本来就不会进入人眼核对环节。
     **fail closed**：未展示向导窗却收到 `pair_confirm` → Receiver **必须拒绝配对**
     （程序侧 SAS 比对只能证明两端算出同一个数，证明不了「用户确实看过并核对过」，
     不能用它替代人眼核对）。
   - **v1.5 增补（Stage 5 第五轮）· 双方同意闸门 + 人眼核对硬闸门**：
     - Receiver 受理**新设备**（自身设备表中没有该 Sender fp）的配对请求时，
       **必须处于待命态**（§2）；否则 MODE 帧带 `err:{reason:"receiver_not_ready"}`，
       **不得弹窗**、不得进入 SAS 阶段。
     - Receiver 收到 `pair_confirm` 后**不得立即接受**：必须等到**本端用户点击「一致」**。
       未表态（用户不点 / 点「不一致」 / 超时 / 被新会话取代）→ **一律拒绝配对**。
       旧实现里 Receiver 的「一致」只是一行日志（实际闸门只有 Sender 端），
       Receiver 端的人眼核对形同虚设 —— 现在两端都是硬闸门。
     - Receiver 的向导窗**关闭时机**由「收到 pair_confirm」改为「用户点完」，
       否则用户连点「一致」的机会都没有。
   - **v1.5 新增（Stage 5 实测确立）· 向导窗单实例**：Receiver **同一时刻只允许一个配对向导窗**。
     新的配对会话到来时，旧向导窗必须**确实销毁**（不是只标记关闭）之后才允许开新的；
     「关闭请求早于窗口创建」的竞态必须被显式处理，否则旧窗口会永久残留在屏幕上
     （Stage 5 实测：屏幕上同时挂着两个配对确认窗，两个 SAS 还不同）。
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
  reason:"ext_rejected"}`（**v1.4：实现已统一为此字符串**——原实现发 `invalid_name`，Android 侧不按
  该字符串分支，改名无兼容风险）；
- **v1.4 新增**：上述四类接收端错误（`ext_rejected` / `io_error` / `disk_full` / `file_too_large`）
  **必须同时给用户可见反馈**（PC 端日志 + 合并限流的消息提示），不得只回 ACK 静默丢弃；
- **隔离写入**：先写 `Received\.tmp\<file_id>.part`，FILE_END 后校验整文件 SHA-256 =
  file_id → 通过则**原子 rename** 到 `<unixMs_Receiver时钟>_<消毒名>`（冲突加 -1/-2）；
  校验失败删 .part 回 transient；
- 落盘名时间戳用 **Receiver 时钟**；跨设备时间比较禁令 [P2-01]。
- heic 在资源管理器无缩略图 → README/托盘提示说明 [P2-02]。
- **v1.5 新增（Stage 5 第三轮实测）· 逐张回执**：Sender 收到 `ACK{status:"ok"}` 后必须把该文件
  记为「**已到电脑**」，并在 UI 上可查（累计张数 + 最近一张的文件名与时间）。
  此前只有「最近同步：成功 / 失败」这种**汇总**回执，用户在手机上**看不出这一张图到底到没到电脑**。
  失败同样要有回执（文件名 + 原因），且成功一张后应清掉上一条失败提示（只反映当前状态，不是历史）。

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

#### 7.3.1 就绪判据与两种「读不到」 [v1.4 新增]
入队只记 URI；**读文件发生在发送时**（入队那一刻文件可能还在写）。三路读取取先成功者
（File 直读 / `openFileDescriptor` / `openInputStream`），全部满足才算就绪：
- **内容完整**：JPEG 末尾 EOI(`FFD9`) / PNG 末尾 `IEND`——MIUI 会在文件写完前就插
  MediaStore 行（实测截断 130126/160387 通过了纯长度比对，只有内容判据能拦住）；
- **长度稳定**：读前 length == 读后 length（两者皆 > 0 时）——仍在增长则判未就绪；
- **长度相符**：bytes 非空，且（expected 未知 或 bytes == expected）。

「读不到」**必须分成两类，不可一律当作未就绪**：

| 情况 | 判据 | 处置 |
|---|---|---|
| 文件还没写完 / 暂不可读 | MediaStore 行**还在** | 保持 `pending`，下轮重试，**永不丢弃**（防丢图） |
| URI 已失效 | MediaStore 行**已删除**（查得 0 行） | 标 `dropped`（`drop_reason=uri_dead`），不再重试，并通知用户 |

查询抛异常按「未失效」处理（保守：宁可继续重试，也不误丢照片）。
未就绪日志限流：前 3 次每次告警，之后每 20 次告警一次（避免长期不可读的文件刷爆日志）。

### 7.4 发送循环
队首 → §2 **三通道**解析 → 连接 → 握手（重连模式）→ file_meta/chunks/end → ack → 记账。
失败回队列，指数退避 30s→1m→5m→15m 封顶；permanent → dropped。

### 7.5 前台服务与保活 [P0-13][P1-07][R-11][R-16]
- FGS 类型：`specialUse`（API 34+；**API 33 走 `dataSync`**——minSdk 33 起 API 31~32 的 dataSync
  降级分支不适用）。Android 15 上 6h/24h 累计超时 → `onTimeout()` → 重启服务；**连续超时 3 次 →
  降级 Alarm-only 模式**（不启 FGS），通知栏提示授予省电白名单）[R-16]；
  **`startForegroundService` 后必须 5 秒内调用 `startForeground()`**（否则系统抛异常）[R-11]；
- **v1.4 新增（Stage 4 实测确立）**：`startForeground()` **必须无条件调用**，绝不因任何历史标志
  而跳过——跳过 = 30 秒后 `ForegroundServiceDidNotStartInTimeException` 杀进程 → 通知随进程消失
  → 状态栏图标整体消失。判据：**抛异常 = 真被拒；不抛 = 成功**（`activeNotifications` 复核实测为
  假阴性，会导致重复图标，禁止使用）。
- 厂商杀后台**不调用 onTimeout** → 自恢复：`START_STICKY` + onStartCommand 重注册 Observer +
  精确闹钟自检心跳（`setExactAndAllowWhileIdle`，`USE_EXACT_ALARM`，见 §7.7）；
- "服务被杀/后台受限"状态显式可见：通知栏"已暂停，点此恢复"；
- **息屏三级保障**：① FGS + Observer（前台/近期活跃时秒级）；② 精确闹钟心跳
  （**MIUI 上进入 deviceidle 白名单后该闹钟不再被挂起**，Doze 下真实节拍可到分钟级；
  未放行时可能被 `Pending user blocked background alarms` 挂起）；③ 省电白名单 + 厂商自启动引导
  （分厂商页 + 完成回读，未完成显示"后台受限"红标）。**对外口径："最终一定到，息屏时可能迟到"。**

### 7.6 开机流程 [P0-12]
**v1.4 按实测改写**（见 §11 偏离 4）：targetSdk 36 触发 BOOT_COMPLETED FGS 禁令，
`BootReceiver` 收到开机广播后**在广播内直接起服务**（`startForegroundService`），**同时**挂
`setExactAndAllowWhileIdle` 精确闹钟作兜底（10 秒后）。
- 依据：MIUI 实测 `Background started FGS: Allowed [... code:SYSTEM_ALLOW_LISTED ...]` —— 白名单
  用户在开机路径上同样被放行，且广播直起比"等闹钟"少一个 10 秒的迟到窗口；
- 降级链：广播内 `startForegroundService` 被拒 → 退化为普通后台服务（`startService`）→
  再被拒才发 `NOTI_ID_DOWN` 提示（不占状态栏）。
- 覆盖安装链（`MY_PACKAGE_REPLACED`）同构：广播直起 + 5 秒后精确闹钟兜底。

### 7.7 权限清单 [P1-06][P1-08][S-01]
`READ_MEDIA_IMAGES`、`READ_MEDIA_VISUAL_USER_SELECTED`（Android 14+ 三态检测：部分授权
→ 明确提示"仅能访问 N 张"并引导全选，**v1.4 已实现**）、`POST_NOTIFICATIONS`（运行时申请）、`INTERNET`、
`ACCESS_NETWORK_STATE`、`ACCESS_WIFI_STATE`（读 SSID/网络状态）、
`FOREGROUND_SERVICE`、`FOREGROUND_SERVICE_SPECIAL_USE`（34+）/
`FOREGROUND_SERVICE_DATA_SYNC`（33 降级）、`RECEIVE_BOOT_COMPLETED`、
`REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`、`ACCESS_LOCAL_NETWORK`（预埋）。
**可选增强**：`ACCESS_FINE_LOCATION`（仅让 SSID 提示更精确；**不申请也完整可用**——
读不到 SSID 时指纹自动降级为网关 IP/子网判据，见 §2；不为一个提示申请位置权限）。
**v1.4 按实测改写**：`SCHEDULE_EXACT_ALARM`（≤API 32，需用户授权）**不申请**；API 33+ 使用
`USE_EXACT_ALARM`（安装即授予）——MIUI 实测该权限 + 电池优化白名单是把后台 FGS 变为
`SYSTEM_ALLOW_LISTED` 永久放行的必要条件之一（见 §11 偏离 3）。`ACTION_SEND` 分享目标：M2。

### 7.8 网络指纹（存储落点）[P2-c]
配对成功时写入 app 私有 config：`{ssid, gateway_ip, paired_at}`；每次网络切换事件更新；
发送前读取并按 §2 判定。不走队列，不落图库。

## 8. PC 端（Receiver）

- **托盘**：`fyne.io/systray`（定案，不用 walk——cgo/MinGW 冲突）；向导/弹窗用
  `x/sys/windows` 原生 Win32；构建脚本 **`CGO_ENABLED=0`** [P1-10]；
- 菜单 = 打开接收文件夹 / **接收文件夹…（改保存目录）** / 重新配对 / **导出诊断信息** /
  修复防火墙规则 / 开机自启(勾选) / 退出；
  - **v1.4**：诊断项由「复制诊断信息」改为「**导出诊断信息**」——写入
    `logs\diagnostics-<时间戳>.txt` 并弹窗告知路径，**不触碰系统剪贴板**（复制语义会
    `EmptyClipboard` 且不还原，直接丢弃用户原内容）；
  - **v1.5 新增（Stage 5 第三轮实测）**：托盘补三个入口，**立即生效、不必改 config.json 再重启**：
    「**剪贴板同步：开/关**」（关掉后图片仍然落盘，只是不写剪贴板）、
    「**剪贴板还原时长：30/60/120/300 秒**」（点击循环切换）、
    「**重启 PhotoBeam**」（改了 config.json 后用；实现上必须**先释放 TCP/UDP 端口**再拉起新进程，
    否则新进程会撞上「端口被占」而弹框退出）；
- **剪贴板（Receiver 侧）**：收到图片 → 写 `CF_DIB` + `PNG`（附排除标记，不进 Win+V / 不上云剪贴板）
  → 窗口期（默认 60 秒）→ 还原**用户原本的内容**。
  - **v1.5 明确（Stage 5 第三轮实测确立）· 还原目标恒定**：**连续写入多张图时，还原目标始终是
    「用户在这轮之前就有的内容」**，绝不能还原成上一张 PhotoBeam 图。
    实现手段 = 写入时带专属归属标记；下一张写入若发现剪贴板里是「本程序上一张图」，
    就**复用第一次写入前捕获的原内容快照**，而不是把上一张图当成原内容。
  - 用户在窗口期内复制了别的内容 → **不还原**（尊重用户操作），且这条与
    「被本程序后续写入取代」是**两种不同的跳过原因**，日志必须能区分开；
  - 剪贴板**无主**（owner 进程已退出）→ 跳过快照、不还原（规避 win32k AV [C-04]），
    这时也不存在「用户原内容」可还原。
- **向导窗**：选保存文件夹（默认 `%LOCALAPPDATA%\PhotoBeam\Received`，OneDrive 重定向检测
  + 警告，配置目录同样避开）→ 显示 SAS → 配对成功；
  - **v1.4 落地要求**：向导窗须**同时**承担三件事——显示 6 位 SAS 码 + 本机指纹 + 手机设备名、
    提供「一致 / 不一致」两个按钮（「不一致」**必须真的发出** `pair_reject`）、提供
    保存目录选择。**只落日志不算实现**；
- **网络自检四件套**（诊断置顶）[P1-11]：对外通告 IP + 所属网卡名（`GetAdaptersAddresses`
  IfType 过滤 + 名称黑名单 Hyper-V/WSL/VMware/Tailscale/WireGuard/Loopback/vEthernet）、
  监听端口、防火墙规则存在性 + **规则路径与当前 exe 路径比对**、网络 profile（公用/专用）；
  另含手机可达 IP、默认网关、认证失败统计 [P1-13]；
- **防火墙**[P1-11]：预置两条规则——TCP 5416-5436 与 UDP 5416，按程序路径绑定、绑定**全部**
  profile；启动自检规则存在 + 路径一致，失效自动重建（UAC，拒绝 → 托盘"修复防火墙规则"
  退路 + 诊断置顶）；发现端口被占 → 明确告警拒绝启动 [P0-11]；
  - **v1.4 落地要求**：端口被占的告警**必须同步弹出**（等用户确认再退出），不得
    `go messageBox(...)` 后立即 `os.Exit(1)`——那会让提示赶不上进程退出，用户只看到"闪退"；
- **常驻**[P1-09][R-17]：**登录自启**（HKCU Run 键，登录时生效——措辞如实）。
  **计划任务看门狗 v1.4 不实现**（Stage 5 决策：不在系统注册计划任务）→ 登记为 §10 延后项；
- 日志：内部环形 200 条 + **崩溃时写退出码与最后 200 条日志**落盘
  `%LOCALAPPDATA%\PhotoBeam\logs` [R-20]；日志需分级别（INFO/WARN/ERR）并按大小轮转；
- **接收反馈**（v1.4 新增）：文件落盘后弹窗须显示**手机端原始文件名**（不是带毫秒前缀的落盘名）
  + 「已保存到 <保存目录>」；`ext_rejected` / `io_error` / `disk_full` / `file_too_large`
  四类错误须给用户可见提示（合并限流，不得逐张弹框）。

## 9. 验收标准（v1.6 修订 · 时延判据收紧）[P0-14][N-07][N-06][R-10]

时延类判据统一"连续 20 次采样 ≥18 次满足"；每条记录实测值+设备+ROM+是否息屏。
1. 配对全流程：**两端同时显示 SAS**；点"不一致"（任一端）→ 双方断开，PC 显示"用户已拒绝配对"；
   （**v1.4**：PC 端向导窗为**必测项**——只有日志不算通过；另需验证 PC 端点「不一致」后
   手机端收到 `pair_reject` 并断开）；
2. **截图 ≤2 秒可粘贴**（20 采样 18 次达标，从按下截图键起算）——
   **v1.6 收紧**：v1.5 为「≤3 秒落盘」，实测端到端（截图→PC 剪贴板可粘贴）为 3.4s，
   未达标。Stage 6 定位到瓶颈在 PC 端关键路径（逐像素转 DIB 278ms + PNG 重编码 846ms
   + 写后固定 sleep 600ms），已重构为「DIB 优先写入、PNG 后台补写」，实测降至 ~1.5s。
   **判据明确为「可粘贴」而非「落盘」**——CF_DIB 出现在剪贴板上即视为达成（用户可 Ctrl+V）。
   守门人：
   · PC 端关键路径预算（无需真机，CI 可跑）：`cd pc && go test -run TestDIB -v`
     ——解码+DIB ≤ 300ms，且 PNG 编码不得回到同步路径；
   · 端到端守门人（需真机已配对）：`pwsh -File tools/assert-latency.ps1`
     ——探针轮询 CF_DIB + adb 触发截图，阈值 2000ms，退出码非 0 即不达标；
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
14. **v1.4 调整**：计划任务看门狗本版不实现 → 本条只验**登录自启**：重启 PC（登录后）→
    PhotoBeam 自动拉起 + 队列补发；「杀 PC 进程 → 5 分钟内自动拉起」一条**不适用**（登记为 §10 延后项）；
15. 手机时钟**调快 2 小时与调慢 2 小时**各测一次 → 落盘与批次标记均不受影响（_ID 水位
    不受时钟影响）[R-09]；
16. 6 次错误配对 → 第 6 次被拒且有提示；一次网络抖动的重连失败**不**触发冷却；
17. `ver:2` 客户端 → PC 回**明文 MODE 携带 `err{reason:"version_unsupported"}`**（不静默断开）；
22. **同一 SSID 的 mesh/多 AP 漫游（BSSID 变化）→ 照片仍能正常发送，不得因 BSSID 变化而阻断**；
    读不到 SSID 时按网关/子网判据降级 [S-01]；
18. 多网卡（WiFi+有线+VPN 虚拟网卡）→ 诊断"手机可达 IP"落在手机同网段网卡，配对成功；
19. Android 14+ 部分媒体授权：选 3 张 → 提示"仅能访问 3 张"并引导全选（**需一台 Android 14+
    真机**，vivo 日常机满足；红米 13 测不到）[R-10]；
    （**v1.4**：三态检测与引导**已实现**，待 Android 14+ 真机补测）；
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

**v1.6 新增延后项（Stage 6 · 范围扩张的代价与约束分析）**：

**延后项 A：保留手机端目录结构（多文件夹 → 多文件夹映射）**

- **目标**：手机 `Pictures/Screenshots/` 落到 PC 的 `Screenshots/`，而不是全部拍平到 `Received/`。
- **协议改动**：`file_meta` 增加 `rel_path` 字段（手机端 `MediaStoreSync` 已在本地读取
  `RELATIVE_PATH`，只是发送时丢弃了）；PC 端落盘前按 `rel_path` 建子目录。
- **代价**：
  1. 协议字段变更 → v1.6 → v1.7，PC 与手机端**必须同版本**，混版会因字段缺失而退化；
  2. **路径穿越风险**（协议 §5.2 已有 `name` 的消毒逻辑，`rel_path` 需同等处理：
     剥离 `..`、绝对路径前缀、盘符、保留字设备名）；
  3. PC 端落盘不再拍平 → 幂等去重键（当前按落盘名）需重新定义，否则同名文件会散落多目录；
  4. 弹窗「已保存到」路径变长，展示需截断处理。
- **约束**：v1 明确「单 PC 配对、单向投递」，目录保留仍是**单向**语义（PC 不回传删除）。
- **建议**：作为 v1.7 的独立小版本实施，不与端到端时延优化混做（避免两个变量同时动，
  无法归因）。

**延后项 B：支持任意文件类型（不止图片）**

- **目标**：同步视频、文档、任意二进制文件到 PC（不再只是剪贴板投递）。
- **协议改动**：
  1. `file_meta` 的 `mime` 与扩展名白名单（现为 `jpg/jpeg/png/webp/gif/bmp/heic`）需放开；
  2. **剪贴板管线不适用**——非图片没有 DIB/PNG 可写。此时 PC 端行为需分叉：
     「落盘但不写剪贴板」，或「按类型决定是否写剪贴板」；
  3. 手机端 `MediaStoreSync` 当前只监听 `Images` 外部内容 URI，需扩展到 `Files`/`Video`
     → **权限模型随之改变**（SAF / MANAGE_EXTERNAL_STORAGE / 分卷 Storage Access Framework）；
  4. 协议 §5.1 的 `ext_rejected` 拒绝分支语义要重新定义（哪些仍拒、哪些放行）。
- **代价与约束**：
  1. **安全面显著扩大**：任意文件类型意味着「手机上的任何文件可被投到 PC 磁盘」，
     需重新审视路径消毒、文件名消毒、磁盘配额、可执行文件（`.exe`/`.apk`）是否放行；
     协议 §9 第 12 条「`.txt`/`.exe` 伪装图片 → permanent 拒绝」就是这条边界的既有表达，
     放开后该条验收标准必须重写；
  2. **权限申请复杂度**：现有「允许访问所有照片」是低摩擦授权，扩到全文件存储是高摩擦、
     可能被用户拒绝、且国产 ROM 审核更严；
  3. **体积与流量**：视频文件动辄数百 MB，30 秒心跳 + 单连接传输模型需要分片续传与
     断点设计（当前协议 §5 的 chunk 机制是单文件顺序流，无断点续传）；
  4. **与产品定位冲突**：PhotoBeam 的定位是「截图 → 剪贴板投递器」，
     任意文件同步是 **Syncthing / SMB / 文件共享** 的职责。若只是需要多目录双向同步，
     重新启用 Syncthing 成本远低于把 PhotoBeam 改造成同步引擎；
  5. 剪贴板语义：非图片文件写入剪贴板没有意义（CF_HDROP 路径列表是另一套机制），
     需要额外定义「只落盘」模式的产品形态。
- **建议**：**不实施**。若确需多目录双向同步，用 Syncthing；若确需「任意文件单向投递到
  指定目录」，可考虑做 PC 端的「文件夹投递箱」——但那本质是另一个产品，不应塞进 PhotoBeam。

**两条延后项的共同判断**：PhotoBeam 当前是**剪贴板投递器**，不是**同步引擎**。
A（目录保留）是低风险、纯 PC 端为主的小改动，值得做；B（任意文件）是产品定位的改变，
成本与收益不成比例，不建议做。

**v1.4 新增延后项**：
- **计划任务看门狗**（§8 原要求每 5 分钟拉起进程）→ Stage 5 决策不实现，登记为后续范围
  （涉及在系统注册计划任务，需用户单独授权）；
- 「导出诊断信息」的稳定文本字段序 → M2 定稿（当前为给人看的自由文本）[P2 备注]；
- 手机端诊断信息的落盘位置（`getExternalFilesDir`）在部分国产 ROM 的"文件管理"里不可见，
  待补充"分享"入口。

---

## 11. 偏离登记（v1.3/v1.4 条款 ↔ 实机行为）

> 修订原则：**以实测为准**。凡 v1.3 / v1.4 条款与真机行为冲突，改协议而非回退已验证有效的实现。
> 每条偏离必须写清：条款位置 / 实测现象 / 处置 / 依据。
> 表头「v1.4 处置」列对 v1.5 的三条（13~15）表示 **v1.5 处置**。

| # | v1.3 条款 | 实机现象 | v1.4 处置 | 依据 |
|---|---|---|---|---|
| 1 | §3.2:124 / §8:287「两端同时展示 SAS（Receiver 向导窗）」；§9.1 验收 | PC 端只有 `logf("配对模式 SAS=%s…")`，**屏幕上没有那 6 位数字**；手机端弹「请在电脑上核对数字」→ 用户无从核对，只能盲点「一致」→ SAS 人眼比对退化为无条件信任 | **条款改为可实现化**：Receiver 必须弹向导窗显示码 + 提供「一致/不一致」，「不一致」必须真发 `pair_reject`；**只落日志视为未实现** | Stage 5 核对（`pc/server.go:262`）；协议 §3.2:128 本就允许双向发 |
| 2 | §8 菜单「复制诊断信息」 | 该项 `EmptyClipboard` 后写文本且**不快照不还原** → 直接丢弃用户原剪贴板内容；且可能被正在跑的还原定时器冲掉 | **改为「导出诊断信息」**：写 `logs\diagnostics-<ts>.txt` + 弹窗告知路径，**完全不碰剪贴板** | Stage 5 决策；`pc/clipboard_windows.go` 剪贴板语义仅用于图片 |
| 3 | §0「`SCHEDULE_EXACT_ALARM` → targetSdk ≥ 33 **不申请、不使用** exact 闹钟」；§7.7 同款表述 | MIUI 实测：精确闹钟链**可用**，且白名单后 AM 放行 `code:SYSTEM_ALLOW_LISTED`（永久许可）；v1.3 的"不申请"是 AOSP 假设 | **改写条款**：API 33+ 用 `USE_EXACT_ALARM`（安装即授予）+ `setExactAndAllowWhileIdle`；≤API 32 的 `SCHEDULE_EXACT_ALARM` 仍不申请 | Stage 4 实测（08:25 白名单前闹钟被 `Pending user blocked` 挂起 → 加入白名单后立即触发） |
| 4 | §7.6 开机流程「BootReceiver **只起**闹钟，闹钟触发时起 FGS」 | 实测 `MY_PACKAGE_REPLACED` / `BOOT_COMPLETED` 广播期间 AM 已放行后台 FGS（`code:PACKAGE_REPLACED` 临时许可 / `code:SYSTEM_ALLOW_LISTED` 永久许可）→ 广播直起可用且少一个 10 秒迟到窗口 | **改写条款**：广播内**直接起服务** + 精确闹钟兜底；并补三级降级链 | Stage 4 实测（09:29:22 `Background started FGS: Allowed`） |
| 5 | §0 `minSdk = 31` | 实际实现 `minSdk = 33`（Android 13+），§7.5 的「API 31-33 dataSync 降级」分支因此不适用 | **改写为 `minSdk = 33`** | `AndroidManifest.xml` |
| 6 | §3.2:122 配对记录在 `devices.json` | 实现写在 `config.json` 的 `devices[]` | **改写条款**为 `config.json`，语义不变 | `pc/config.go` |
| 7 | §5.2:198 ACK `reason:"ext_rejected"` | 实现发 `invalid_name`（Android 侧不按该字符串分支，无兼容风险） | **回改代码**统一为 `ext_rejected` | Stage 5 |
| 8 | §8:296-298 计划任务看门狗（M1 就做） | Stage 5 决策：不注册系统计划任务 | **移出本版范围** → §10 延后项 | Stage 5 决策 |
| 9 | §8:289-292 网络自检四件套 | 诊断文本只有端口/防火墙存在性/设备数/日志尾部 | **保留条款并落地**：补通告 IP+网卡名、默认网关、网络 profile、规则路径比对、认证失败统计 | Stage 5 |
| 10 | §8:299 环形 200 条 + 崩溃写退出码与最近 200 条 | 实现无环形缓冲，panic 只写一行文本 | **保留条款并落地** | Stage 5 |
| 11 | §3.2:129 PC 显示「用户已拒绝配对」 | 实现文案为「手机端已拒绝配对」，且 PC 端**没有**"不一致"动作 | **改写条款**：PC 点「不一致」后 PC 显示「用户已拒绝配对」 | Stage 5 |
| 12 | §7.7 Android 14 三态授权 | 实现只请求 `READ_MEDIA_IMAGES`，选「部分照片」后永远提示"权限未授予" | **保留条款并落地**：三态检测 + 引导全量 | Stage 5 |
| 13 | §3.2:141「判据 = Sender 的 fp 是否在 devices.json」→ Receiver 一律进入配对并展示向导窗 | 手机端**后台同步服务每 30 秒自动重连**一次；PC 端每 30 秒弹出一个新的配对向导窗，把用户正在核对的窗口顶掉（实测 19:11:50 / 19:12:02 / 19:12:32 / 19:13:02 / 19:13:32 … 连续 6 分钟不断弹） | **协议增补 `pair_intent`**（§3.1）：Sender 显式声明本次连接是否由用户主动发起配对；Receiver 只在 `pair_intent=true` 时展示向导窗。字段缺省按 `true` 处理（旧版本行为不变） | Stage 5 实测；`pc/server.go` 配对分支；`android/Protocol.kt` HELLO |
| 14 | §3.2「Receiver 必须弹出向导窗」 | 实现上「同一时刻只允许一个向导窗」只是把旧会话标记关闭（PostMessage WM_CLOSE），**不保证旧窗口真的消失**；且 `close()` 在「窗口已可见但句柄尚未写回」的窗口期读到 `hwnd=0` 就什么都不发 → **旧窗口永久残留在屏幕上**（用户实测：同时出现两个配对确认窗，两个 SAS 还不同） | **条款改写为可验证要求**：同一时刻至多一个向导窗；新会话必须等旧窗口**确实销毁**后才开新的；「关闭请求早于窗口创建」的竞态必须显式处理（窗口先隐藏创建、写回句柄后再显示） | Stage 5 实测；`pc/pairing_windows.go` `showPairingWizard` / `close` |
| 15 | §3.2「Sender 发 pair_confirm → Receiver 验 sas 与签名即接受」 | 只做了程序侧 SAS 比对，**人眼核对（用户看过两端的 6 位码）没有硬性绑定**；且 v1.5 增补 `pair_intent` 后出现「Receiver 未展示向导窗却收到 pair_confirm」的新路径 | **条款收紧（fail closed）**：Receiver 未展示向导窗时收到 `pair_confirm` **一律拒绝配对**——程序侧比对不能替代人眼核对 | Stage 5；`pc/server.go` 配对分支 |
| 16 | §8 剪贴板「窗口期后还原原内容」；§7 批次语义「剪贴板只有一个槽位」 | 实测：连发两张图时，**第二张的「原内容快照」抓到的是第一张 PhotoBeam 图** → 窗口期结束时还原回去的是上一张图，而不是用户原本复制的东西；且「用户复制了别的」与「被本程序后续写入取代」两种跳过在日志里无法区分；托盘里也改不了剪贴板开关/还原时长（只能手改 `config.json` 再重启） | **条款明确 + 实现修正**：① 还原目标恒定为「用户这轮之前就有的内容」（写入带归属标记，检测到「当前是本程序上一张图」就复用首次捕获的原内容快照）；② 两类跳过原因分别记日志；③ 托盘补「剪贴板开关 / 还原时长 / 重启」三个入口，立即生效（重启须先释放端口） | Stage 5 第三轮实测（`pc/clipboard_windows.go`） |
| 17 | §2「三通道发现」；§2 [R-19]「允许手机端手工输入 PC IP」 | ① 代码注释写「仅当 fp 匹配已知设备时才算命中」，**但实现里根本没有做 fp 比对**（两个分支返回完全相同）→ 同一局域网里有别人的 PhotoBeam PC 时，手机可能认错机器；② 手机失败提示让用户「手动输入电脑 IP」，**但 App 里没有任何输入框** → [R-19] 兜底根本没落地，用户被指引到一个做不到的动作 | **条款细化 + 实现补齐**：① §2 增补「应答必须按指纹甄别」的明确规则（别人家的 PC 忽略、同一台换 IP 算命中）；② 手机端提供电脑 IP 输入框，填了就优先走通道①直连 | Stage 5 第四轮（`android/.../Discovery.kt`、`MainActivity.kt`） |
| 18 | §3.2「Receiver 必须弹出向导窗显示 6 位码 + 提供一致/不一致」 | ① **同一局域网里跑着多对 PhotoBeam 用户时，手机首次配对会取「第一个应答」** → 可能连到别人的电脑，之后照片持续串到对方机器；而你的电脑会被陌生人的手机触发弹窗，你点「不一致」还会打断对方的配对；② Receiver 的「一致」**只打一行日志**，收到 `pair_confirm` 就接受 → Receiver 端的人眼核对形同虚设，真正的闸门只有 Sender 端 | **条款改写为「双方同意」**：① 新增待命闸门（§2）：Receiver 只在待命态受理新设备，否则 MODE 帧带 `err{receiver_not_ready}` 且**不弹窗**；`found` 增 `pair_ready` 供 Sender 择优；Sender 的 SAS 界面必须显示对端电脑名；② Receiver 必须等**本端用户点「一致」**才接受 `pair_confirm`，关窗时机也改为「用户点完」 | Stage 5 第五轮（`pc/pairing_state.go`、`pc/server.go`、`android/.../MainActivity.kt`） |
| 19 | §8「计划任务看门狗」（注册系统计划任务） | 注册计划任务**需要提权 / 用户单独授权**，上一轮按决策不做 | **换实现而非放弃**：改用**隐藏监护进程**（同一 exe `--supervise`，命名互斥体保证唯一）盯主进程；主进程异常退出即拉起，**不需要任何授权**；托盘可开关；托盘「退出」正常关闭不会被拉起 | Stage 5 第五轮（`pc/supervisor_windows.go`） |
