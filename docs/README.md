# PhotoBeam 文档索引

本目录是 PhotoBeam 的协议规范、各阶段证据与审核记录。**实现唯一依据是 `protocol-v1.md`**。

**第一次用这个软件 → 看 [`user-journey.md`](user-journey.md)**（按步骤走的用户操作手册，含状态区释义与故障自查）。

| 文档 | 内容 |
|------|------|
| `protocol-v1.md` | 传输协议规范 **v1.4**（帧格式、三通道发现、握手、批次与时序；§11 登记 v1.3↔实测的全部偏离） |
| **`user-journey.md`** | **用户操作手册**：首次安装配对 → 日常使用 → 出问题自查 → 已知限制 |
| `handoff.md` | 交接说明：模块边界、协议约定、构建与运行方式 |
| `delivery-status.md` | 交付状态快照 |
| `m1-status.md` | M1 里程碑状态 |
| `stage1-evidence.md` / `stage1-recheck-evidence.md` / `stage1-recheck2-evidence.md` | Stage 1（传输主干）证据与两轮复检记录 |
| `stage2-evidence.md` / `stage2-review.md` | Stage 2（剪贴板 + 弹窗）证据与审核报告 |
| `stage3-evidence.md` / `stage3-review.md` | Stage 3（加固 + 打包）证据与审核报告 |
| `stage4-evidence.md` | Stage 4（图标 / 托盘 / 弹窗 / 传输动画 / 后台自起）完整证据 |
| `stage5-evidence.md` | Stage 5（**使用场景全流程核对 + 交互断裂修复**）：逐步复核、断点清单、协议偏离登记 |

## 关键图（`images/`）

| 文件 | 内容 |
|------|------|
| `images/22_three_icons_consistent.png` | 启动器 / 状态栏 / 托盘 三处图标一致对比 |
| `images/26_launcher_icon_v2.png` | Android 启动器实机图标（P 居中放大） |
| `images/13_launcher_icon_zoom.png` | 启动器图标修复前后对照 |
| `images/28_pc_tray_icon_v2_zoom.png` | PC 托盘图标（12× 放大） |
| `images/32_p_on_zoom.png` | 状态栏：已连接（亮白空心 P） |
| `images/33_p_off_zoom.png` | 状态栏：未连接（暗 P，服务仍在） |
| `images/34_p_reconnect_zoom.png` | 状态栏：PC 重连后恢复亮白 |
| `images/18_upload_anim_levels.png` | 传输动画 4 帧波点的纵向档位 |
| `images/04_pc_popup_thumb.png` | PC 弹窗缩略图 + 文件名省略号 |

## 脱敏说明

文档中的本机标识已统一替换为占位符，**不改动任何技术结论与数据**：

| 占位符 | 原含义 |
|--------|--------|
| `<user>` | 本机 Windows 用户名 |
| `<PC-NAME>` / `<PC-IP>` | 接收端 PC 的主机名 / 局域网地址 |
| `<PHONE-IP>` / `<PHONE-MODEL>` / `<VENDOR>` | 手机地址 / 型号 / 品牌 |
| `<PC-FP>` / `<PHONE-FP>` | 配对指纹（设备身份密钥派生值） |
| `<ADB-SERIAL>` | adb 设备序列号 |
| `<USER-DIR>` | 手机相册下用户自建目录名 |

## 原始证据的归档位置

实机截图 / 录屏 / 逐帧哈希 / 探测图共 **123 MB、402 个文件**，且含本机桌面与设备信息，
**不随仓库分发**（见根目录 `.gitignore`），仅保留在本机 `docs/stage4-screenshots/`。

文档中所有结论均可在真机上重算——命令见 `stage4-evidence.md` §11「复现要点」，
图标与动画的量化方法见 `tools/README.md`。
