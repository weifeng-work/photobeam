# PhotoBeam

手机截图/图片 → 局域网 → Windows 剪贴板的极简自研工具。
拍完照，电脑上直接 Ctrl+V。不经过任何云，数据不出局域网。

协议规范：`docs/protocol-v1.md`（**v1.4**）
使用手册（按步骤走，含已知限制）：**`docs/user-journey.md`**

## 安装与使用

> 本仓库只收录**源码与文档**，不含预编译产物（`PhotoBeam.apk` / `dist/PhotoBeam.exe`）——
> 请按下方「构建」自行生成，工具链获取见 `tools/README.md`。

### PC（Windows 10+ x64）

1. 双击 `dist/PhotoBeam.exe`（单文件，免安装；首次运行会请求一次 UAC 配置防火墙规则）
2. 托盘出现 PhotoBeam 图标即就绪（TCP/UDP 5416，5416-5436 自动探测）
3. 本程序**没有主窗口**，一切操作在托盘图标上（左键/右键同一个菜单）

### 手机（Android 13+）

1. 安装 `PhotoBeam.apk`
2. 打开应用，按提示依次完成：
   - **① 授权照片与通知访问**（Android 14+ 请选「允许访问所有照片」）
   - **⑤ 允许后台常驻（关闭电池优化）** ← 不做会导致开机后图标长时间不亮
   - **② 配对电脑（核对 6 位数字）** ← 电脑上会弹出配对窗显示 6 位码，**两端核对一致**再点「一致」
3. 之后**什么都不用做**：截图即可自动同步

其余按钮：**③ 立即同步**（手动催一次）、**④ 启动/刷新后台同步服务**（状态降级时用）、
**⑥ 停止同步服务**、**⑦ 导出诊断信息**。

### 日常使用

- 手机截图 → 约 1 秒后 PC 剪贴板可粘贴，右下角弹窗提示，60 秒后自动还原原剪贴板
- 写入的图片不进 Win+V 历史、不上云剪贴板（微软官方排除标记）
- 手机通知栏图标**亮白 = 已连接**、**变暗 = 未连接**（服务仍在，图片入队，恢复后自动补发）；
  **图标整体消失 = 服务真的没运行**（不会因为"没连上"就消失）
- 排查问题：电脑托盘 →「导出诊断信息」→ 生成 `logs/diagnostics-*.txt`（**不碰剪贴板**）；
  手机端 App 内也有「⑦ 导出诊断信息」

## 构建

| 产物 | 命令 | 说明 |
|------|------|------|
| `dist/PhotoBeam.exe` | `bash build-pc.sh` | 发布版，GUI 子系统，PE 校验内置 |
| `PhotoBeam.apk` | `build-android.bat`（或 `bash build-android.sh`） | 已 zipalign + apksigner |

PC 开发调试版：`cd pc && go build -o PhotoBeam.exe .`（控制台子系统）；
`PhotoBeam.exe --test-clipboard <图片路径>` 离线自检剪贴板管线，输出在
`%LOCALAPPDATA%\PhotoBeam\logs\pc.log`。

## 目录结构

```
pc/          Go：托盘 + 配对 + UDP 发现 + TCP 接收 + 剪贴板管线 + 弹窗（零 CGO，单文件）
android/     Kotlin：图库监听 + 发送 + 配对 + 前台服务（无 AndroidX，直连工具链构建）
design/      图标母版（字形与底色的唯一真源）
docs/        协议规范、各阶段证据与审核报告（索引见 docs/README.md）
docs/images/ 精选关键图（已脱敏，可公开）
tools/       图标生成、测试图生成、录屏/截图量化脚本（见 tools/README.md）
```

不入库的目录（见 `.gitignore`）：`dist/`、`android/build/`、`tools/kotlinc/`（工具链）、
`docs/stage4-screenshots/`（123 MB 原始实机证据）、`.lam/`（多智能体编排内部件）。

## 升级须知

- APK 用 `release` 签名（keystore 在 `%LOCALAPPDATA%\PhotoBeam\photobeam-release.keystore`，
  口令在 `keystore.pass` 或环境变量 `PHOTOBEAM_KEYSTORE_PASS`）。**换机/重装请务必备份这对钥匙**；
  丢失后无法对已装设备做覆盖升级（签名不同只能卸载重装）。
- PC 端为单文件 exe，直接替换即可；配置与身份保存在 `%LOCALAPPDATA%\PhotoBeam\`。

## 交付状态

- Stage 0（协议）四轮审阅签字；Stage 1（传输主干）签收；Stage 2（剪贴板+弹窗）签收；
  Stage 3（加固+打包）签收；Stage 4（图标 / 托盘 / 弹窗 / 传输动画 / 后台自起）为当前交付版
- Stage 4 关键结论：**状态栏图标可见 ⟺ 同步连接可工作**。覆盖安装 / 开机自起时服务
  必然建立真前台服务通知（`startForeground` 无条件调用，不因历史标志跳过），
  状态栏 P 恒可见，**亮 = 已连接、暗 = 未连接**，断连与恢复双向实测闭环；
  详见 `docs/stage4-evidence.md` §6。
- 已知边界：单 PC 配对（v1 范围）；60 秒窗口内用户复制了别的内容则不还原（尊重用户操作）；
  剪贴板内容属已退出进程时不做还原（规避 win32k 崩溃，还原无意义）；
  MIUI 上需用户一次性加入电池优化白名单，否则开机链可能延迟到心跳周期（App 内有引导）。

## 仓库内容与隐私

- 仓库内**不含任何密钥、证书、口令或设备身份材料**：release keystore 存在仓库外
  （`%LOCALAPPDATA%\PhotoBeam\`），本仓库只引用其路径。
- 文档中的本机标识（用户名、主机名、局域网 IP、手机型号、配对指纹、adb 序列号）
  已统一替换为占位符，见 `docs/README.md`「脱敏说明」。
- 实机截图、录屏、逐帧哈希与探测图（123 MB / 402 文件）含本机桌面与设备信息，
  **不随仓库分发**，仅保留在本机 `docs/stage4-screenshots/`。

