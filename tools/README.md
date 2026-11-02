# tools/ 构建与验证工具

## 一次性依赖（不入库，需自行获取）

| 文件 | 用途 | 获取方式 |
|------|------|----------|
| `tools/kotlinc/` | Kotlin 编译器 | 解压 `kotlin-compiler-*.zip` 到 `tools/kotlinc/`，需含 `bin/kotlinc.bat` 与 `lib/kotlin-stdlib.jar` |
| `tools/bcprov.jar` | BouncyCastle Provider（kotlinc 编译期 classpath） | Apache BC 发行包中的 `bcprov-jdk18on-*.jar`，重命名为 `bcprov.jar` |
| `tools/kotlinc.zip` | 上者的下载缓存 | 可选 |

Android 构建的其余前置（路径见 `build-android.sh` 顶部）：JDK 21、Android SDK
（`%LOCALAPPDATA%\Android\Sdk`，需 build-tools 37.0.0 与 platform android-36）。

## 脚本

| 脚本 | 用途 |
|------|------|
| `gen-app-icons.py` | **统一图标生成器**：母版 / 设计总览 / `pc/icon.ico` / Android 各密度传统图标与自适应前景（幂等可重跑） |
| `extract-icon-parts.py` | 从母版一次性抽出 `icon_glyph_p.png` + `icon_shape.png`（含顶边毛刺裁除） |
| `gen-test-images.py` | 生成 N 张**内容互异**的测试图（PC 侧按 SHA-256 去重，内容相同会被判 duplicate 而不落盘） |
| `capture-upload-anim.ps1` | 传输动画一键验证：生成 → 推送 → 批量定稿 → 连拍 → 帧哈希 |
| `analyze-upload-anim.py` | 录屏抽帧 + 波点纵向位置量化（判定 4 帧是否真在轮转） |
| `capture-notif-frames.ps1` | 状态栏图标帧哈希对比（判定静止 / 在动） |
| `capture-tray-icon.ps1` | 托盘图标 UIA 定位 + tooltip 佐证抓图 |
| `capture-popup.ps1` | PC 弹窗抓图 |
| `watch-popup-window.ps1` | 弹窗窗口监视诊断 |
| `enum-tray.py` | Win32 托盘按钮枚举（`ReadProcessMemory` 读 tooltip 文本） |

## Python / 外部依赖

- Python 3.12+，`Pillow`、`numpy`
- `ffmpeg` / `ffprobe`（仅 `analyze-upload-anim.py` 的录屏抽帧需要）
- `adb`（Android 侧推送 / 截图 / logcat）
- Windows：`build-pc.bat` 需要 Go 工具链；`.bat` 文件保持纯 ASCII（cmd.exe 按 OEM 代码页解析）
