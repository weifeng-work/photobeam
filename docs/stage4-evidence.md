# PhotoBeam Stage 4 证据（图标 / 托盘 / 弹窗 / 传输动画）

- 任务 marker：`lam-20261102-045427-fcbd`
- 工作目录：`C:\Users\<user>\Project\PhotoBeam`
- 交付物：`dist\PhotoBeam.exe`（4,857,344 B，GUI 子系统）+ `PhotoBeam.apk`（release，证书 `fc1596ed855ebbe1f642ab4f54a7b45856b7aa04019aaf46d25305e493ca1821`）
- 实测环境：PC `<PC-NAME>` / 192.168.0.<PC-IP>（PhotoBeam PID 36568）；手机 <VENDOR> <PHONE-MODEL> / Android 13 / SDK 33
- 配对：PC 指纹 `<PC-FP>` ↔ 手机指纹 `<PHONE-FP>`（PC 日志「配对成功: <PHONE-MODEL> (<PHONE-FP>)」）

本轮 4 个验收点的结论：**全部达成**，其中 2 个的原始结论被实测推翻并修正（见 §1.1、§3.1）。

---

## 0. 验收点总览

| # | 验收点 | 状态 | 关键证据 |
|---|--------|------|----------|
| 1 | PC 弹窗缩略图显示 | ✅ 通过（根因与交接结论不同） | `04_pc_popup_thumb.png`、PC 日志 `StretchBlt ... 返回=1` |
| 2 | PC 弹窗文件名省略号 | ✅ 通过（flags 修正为 0x8024） | `04_pc_popup_thumb.png` |
| 3 | Android 传输动画视觉验证 | ✅ 通过（4 帧波点轮转，量化到 5 档纵向位置） | `18_upload_anim_levels.png`、`anim9_video\anim_timeline.txt` |
| 4 | 三处图标一致 + 桌面图标可读 | ✅ 通过（v2：P 居中 + 放大，桌面/托盘实机实测偏移 ≤0.5px） | `22_three_icons_consistent.png`、`26_launcher_icon_v2.png`、`28_pc_tray_icon_v2_zoom.png` |
| 5 | **状态栏图标可见 ⟺ 同步连接可工作**（用户追加） | ✅ 通过（根因与交接结论相反：是**我们自己**跳过 `startForeground()` 导致进程被系统杀，不是系统拒绝） | `32_fgs_v2_statusbar_crop.png`、`33_fgs_v2_off_icon_crop.png`、§6 |

> **v2 追加（用户反馈后）**：桌面 P 太小、托盘 P 不居中 → 统一版式为「P 居中 + 放大」，并把托盘 `.ico` 与 Android 图标收敛到同一生成器（`tools\gen-app-icons.py`），详见 §4.3。
> **§6 追加（用户再次反馈）**：覆盖安装/开机自起时状态栏图标整体消失 → 三层根因逐层实测钉死并修复，详见 §6。**§6 推翻了上一版写进文档的结论**，以本节为准。

---

## 1. PC 弹窗缩略图（`StretchDIBits` 画不出）

### 1.1 根因：**交接结论被实测推翻**

交接件给的结论是「负源矩形高度是根因」。本轮用对照实验 `pc\dibtest3\main.go` 逐条排除，**该结论不成立**：

| 变量 | 变体 | `StretchDIBits` 返回扫描行数 |
|------|------|------------------------------|
| DIB 内容 | 合成的同尺寸 DIB | 正常绘制 |
| DIB 内容 | **真实图像 DIB（1080×2340 bottom-up 32bpp）** | **0（一行都没画）** |
| 缩放比 | 1:1（不缩放） | **0** |
| `biHeight` | top-down / bottom-up / 负源高 | **0 / 0 / 0** |

即：本项目这种「大尺寸 bottom-up 32bpp DIB」喂给 `StretchDIBits` 会**返回 0 扫描行**，与缩放比、与 `biHeight` 正负都无关。`dibtest3` 同时验证了 `CreateDIBSection + StretchBlt` 在同样输入下**稳定成功且方向正确**（`pc\dibtest3\dibtest3_stretchblt.png`）。

### 1.2 改动（`pc\popup_windows.go`）

- `StretchDIBitsTopDown` 改为 **`CreateDIBSection + StretchBlt`**：新建可写的 32bpp 目标位图，把调用方 DIB 的像素**拷进**本地位图副本再拉伸，**绝不原地改调用方的 `dib[8:12]`**（原实现原地改 `biHeight` 会与剪贴板路径争抢同一块内存，是数据竞争）。
- 新增 proc：`CreateCompatibleDC` / `CreateDIBSection` / `StretchBlt` / `DeleteDC` / `DeleteObject`；删除 `procStretchDIBits`。
- 弹窗状态原子化：新增 `showPopupEx` 单一入口，缩略图状态 `has/imgW/imgH` 与文字在同一个 `popupMu` 临界区内一次性设置。
  - 旧实现有**顺序 bug**：`showPopupThumb` 置 `popupThumb.has=true`，紧接着的 `showPopup` 又把它置回 `false`，`WM_PAINT` 永远进不了缩略图分支 —— 即便 GDI 正常也画不出来。
- `PAINTSTRUCT` 缓冲由 `ps[72]byte` 修为 `ps[80]byte`：x64 上 `PAINTSTRUCT` 实为 80 字节，原代码越界写栈 8 字节。
- 删除未使用的 `encoding/binary` 导入。

### 1.3 实测

PC 日志（真实传输时打印）：

```
[2026-11-02 06:44:18] PhotoBeam 弹窗: 缩略图已挂载 1080x2340 dib=10108840B
[2026-11-02 06:44:18] PhotoBeam 弹窗缩略图: StretchBlt dst=(6,9,36x80) src=(0,0,1080x2340) 返回=1
```

截图 `docs\stage4-screenshots\04_pc_popup_thumb.png`：弹窗左侧 36×80 缩略图**方向正确**（未上下翻转），右侧文件名省略号可见。放大图 `08_popup_left_zoom.png` 为裁剪失败态的旧证据，仅作对照。

---

## 2. PC 弹窗文件名省略号

### 2.1 flags 笔误定位

| 值 | 组成 | 结论 |
|----|------|------|
| 原 `0x5024` | `DT_PATH_ELLIPSIS(0x4000)` + `0x1000`（**无效位**）+ `DT_SINGLELINE(0x20)` + `DT_VCENTER(0x04)` | `0x1000` 是笔误；`DT_PATH_ELLIPSIS` **只对路径字符串生效**，普通文件名不产生省略号 |
| 交接建议 `0x4024` | `DT_PATH_ELLIPSIS` + `SINGLELINE` + `VCENTER` | **仍不对**，同上 |
| 实际采用 `0x8024` | `DT_END_ELLIPSIS(0x8000)` + `SINGLELINE(0x20)` + `VCENTER(0x04)` | ✅ 普通文本正确出省略号 |

### 2.2 实测

`04_pc_popup_thumb.png` 中文件名呈 `1793570325699_PhotoBeam_final_01_Jon…`，省略号可见。

---

## 3. Android 传输动画（传输中才动）

### 3.1 原实现存在 **3 个叠加缺陷**（均实测定位，非推测）

1. **首帧永不出现**：`setUploading(true)` 只把 `animTick` 排到 180ms 之后；而单张实测传输仅 **127ms**，`animTick` 一次都没跑 → 传输中状态栏仍是普通白 P，"上传动画"从未出现过。
2. **level-list 不重绘**：`setSmallIcon(同一资源 id, 不同 level)` 在 SystemUI 侧按**资源 id** 缓存，level 变化不触发状态栏重绘 → 帧切换显示不出来。
3. **上传态被切碎**：MediaStore 每次变更都立刻 `reconcile + flush`，每张图单独建一次连接（实测每次 ~100ms），`uploading` 被切成上百个碎片，180ms/帧的动画永远停在第 0 帧。

### 3.2 修复（`android\...\SenderService.kt`）

- `setUploading(true)` **立即** `startForegroundCompat(0)` 刷出第 0 帧（任何长度的传输都看得到上传态）。
- 新增 `UPLOAD_FRAMES = intArrayOf(R.drawable.ic_stat_up_0, ..._1, ..._2, ..._3)`，**逐帧用 4 个独立 drawable**（资源 id 变化 ⇒ SystemUI 必然重绘），180ms/帧。
- MediaStore 变更改为 **250ms 去抖合并**（`flushRunnable`）：一次冲刷把这一批图放在**同一条连接**里发完，`uploading` 持续数秒。发送延迟 +250ms，仍在协议 §9「截图 ≤3 秒落盘」之内。

### 3.3 实测（录屏 + 帧量化）

设备端 `screencap` 全屏 PNG 单帧耗时约 1.9s（采样太稀），改用 **`screenrecord` 录屏 + ffmpeg 抽帧**：

- 录制：`docs\stage4-screenshots\pb_anim9.mp4`（1080×2400，50s）
- 抽帧：`ffmpeg -vf fps=5,crop=62:60:108:30`（状态栏 PhotoBeam 图标区）
- 逐帧量测「P 右侧波点的纵向中心」，4 帧动画只差波点高度 → 直接可量化

`docs\stage4-screenshots\anim9_video\anim_timeline.txt`（节选）：

```
c0001..c0087   IDLE-P          ← 传输前，空闲静止
c0088..c0143   L1/L2/L3/L4/L5 ← 传输中，波点 5 个纵向档位
c0144..c0227   IDLE-P          ← 传输结束，回到纯空心 P
```

波点纵向位置聚类：

| 档位 | y | 帧数 |
|------|---|------|
| L5 | 32.0 | 12 |
| L3 | 34.0 | 12 |
| L2 | 36.0 | 13 |
| L4 | 38.0 | 27 |
| L1 | 40.0 | 13 |

**证据图 `docs\stage4-screenshots\18_upload_anim_levels.png`**：5 档横向并排，可直接看到「空心 P + 3 个圆点」在 5 个不同高度上轮转（即 `ic_stat_up_0..3` 四帧动画 + 过渡帧）。

### 3.4 空闲必须静止

`frames\idle3\frames.csv`：16/16 帧图标区域 SHA-256 **全同**（`96000B48A9FE`）→ 判定 `STATIC`。空闲不闪、只在传输中动，符合验收口径。

### 3.5 批量触发方式（实测要点）

- 一次 `adb push` 40 张**内容互异**的图 → 设备 `/sdcard/Pictures/pb_anim_<tag>/`；再 `content call --method scan_volume` 一次性定稿 → MediaStore 批量变更 → 应用 250ms 去抖合并成**一次冲刷、一条连接**。
- 40 张图实测传输 **8～12 秒**（PC 侧剪贴板 300ms/张限速所致），`uploading` 持续足够长。
- **坑 1**：文件名必须每批唯一。同名文件会**复用 MediaStore 旧 `_id`**，而应用按 `_id > 水位` 过滤 → 实测「对账新增 0」，整批漏收。
- **坑 2**：图片内容必须互异。PC 侧按 SHA-256 `file_id` 幂等去重，内容相同会被判 `duplicate`，不产生真实传输。

---

## 4. 桌面（自适应）图标

### 4.1 v1 缺陷（已修）

旧前景 `res\mipmap-xxxhdpi\ic_launcher_foreground.png`（432×432）中，空心 P 位于 **x=126..204, y=9..192**（偏左上）。自适应图标只露**中心 72dp**（432px 画布里即 144px），P 顶部约 1/3 落在可见区之外 → 被启动器蒙版裁掉，启动器实测渲染为**半截 P**（`13_launcher_icon_zoom.png`，旧态证据）。

### 4.2 v1 修复（锚定到可见区，仍偏小）

新增 `tools\gen-launcher-icons.py`：以设计母版 `design\icon_master_512.png` 为唯一字形来源（**字形不重画**），只把 P 按母版比例锚定到蒙版可见区（左 24.5%、顶端齐平），补齐 5 个密度。**P 不再被裁，但实机只有图标高度的 34%，偏小**。

### 4.3 v2 修复（用户反馈驱动：**P 居中 + 放大**）

> 用户反馈：「图标在手机桌面上显示的 app 图标不正常，字母 p 太小」「电脑托盘里面的图标也有问题，字母 p 不是显示在图标正中间」。

两项诉求同源：旧版式把 P「顶端出头挂在左上角」，这在托盘（无蒙版、16px 小图）上就是明显偏心，在启动器上则因 MIUI 会二次缩放前景层而显得更小。

**新版式（唯一真源，`tools\gen-app-icons.py`）**：

| 项 | 值 | 说明 |
|----|----|------|
| 底图 | 深色圆角方块 `#1A1A1A` | 来自 `design\icon_shape.png`（已剔除原母版顶边毛刺） |
| 字形 | 白色空心 P | 来自 `design\icon_glyph_p.png`（**字形不重画**，宽高比 0.427） |
| 布局 | **水平 + 垂直居中** | P 中心 = 图标中心 |
| P 高度 | 传统图标 / PC 托盘 **66%**；自适应前景 **78%** | 自适应取大值：MIUI 会二次缩放前景层（实测渲染出的 P 只有图标高度的 34%，而源文件里 P 占 72dp 可见区的 66%），且前景仍受 66dp 安全圆约束（上限 84%，取 78% 留余量） |

**顶边毛刺**：原母版顶边有一块 72 列宽的窄条凸起（x≈118..179 高出平顶边 16px），是绘制残留而非设计元素。`tools\extract-icon-parts.py` 按平顶边（众数 y）裁除。

**改用统一生成器的原因**：托盘 `pc\icon.ico` 与 Android 图标同源于母版，之前是两套互不相干的代码路径（`.ico` 是历史手绘产物），无法保证「三处一致」。现在 `design\icon_glyph_p.png` + `design\icon_shape.png` 是唯一真源，`tools\gen-app-icons.py` 一次生成：母版 / 设计总览 / `pc\icon.ico`（7 尺寸）/ Android 5 密度传统图标 + 5 密度自适应前景，幂等可重跑。

> 另注：PIL 的 ICO 插件在 `append_images` 下只写一个条目，`sizes=` 又会覆盖 `append_images`；改为手工写 PNG 条目的 ICO（`ICONDIR` + 7 × `ICONDIRENTRY`），与原文件格式一致。

### 4.4 实测（v2）

| 位置 | P 中心相对图标中心 | P 高度占比 |
|------|-------------------|-----------|
| Android 启动器（实机桌面，160px 图标） | **dx=0.0, dy=-0.5** | **42%**（v1 为 34%，缺陷态约 21%） |
| PC 托盘 `icon.ico` 16/24/32/48/64/128/256 全部 7 个尺寸 | **dx=0.0, dy=0.0**（≤0.5px 属取整） | 66%~69%（v1 16px 时偏心 (-2,-4)） |
| Android 状态栏 | 居中 | ~75%（状态栏图标按惯例填满 24dp 画布） |

证据：`26_launcher_icon_v2.png`（实机桌面裁切）、`28_pc_tray_icon_v2_zoom.png`（托盘 12× 放大）、`25_icons_v2_layout.png`（生成器合成预览）、`27_pc_tray_icon_v2_panel.png`（托盘面板实拍）。

---

## 5. 三处图标一致

`docs\stage4-screenshots\22_three_icons_consistent.png`（v2 实拍）：

| 位置 | 表现 | 素材 | P 居中度 |
|------|------|------|----------|
| Android 启动器 | 深色圆角方块 + 白色空心 P（占图标高 42%） | `26_launcher_icon_v2.png`（实机桌面裁切） | dx=0.0, dy=-0.5 |
| Android 状态栏 | 纯白色空心 P（空闲）；传输中为 P + 上升波点 | `21_notif_icon_zoom.png`（实机状态栏裁切，x=280..290） | 居中 |
| PC 托盘 | 深色圆角方块 + 浅色空心 P（占图标高 66~69%） | `28_pc_tray_icon_v2_zoom.png`（12× 放大） | dx=0.0, dy=0.0 |

三处**同一个字形**（`design\icon_glyph_p.png`，宽高比 0.427）、**同一底色**（`#1A1A1A`）、**同一版式**（P 居中）；比例差异来自各平台渲染机制（状态栏按 24dp 填满、自适应被 MIUI 二次缩放、托盘为 16px 小图）。

托盘图标位置用 Win32 枚举 + UIAutomation 精确定位（不按像素猜），tooltip「PhotoBeam 接收器 · TCP 5416 · 指纹 <PC-FP>」直接证明该图标就是本程序（`27_pc_tray_icon_v2_panel.png`）。

---

## 6. Android 12+/MIUI 后台自起：状态栏图标可见 ⟺ 同步连接可工作

> 用户追加要求：「**保证状态栏的图标可见与同步连接可工作划等号**」——
> 即 Android 12+/MIUI 后台启动前台服务被拦时，状态栏图标**不得整体消失**。

### 6.0 结论先行：上一版结论被实测**推翻**（三次）

上一版文档写的是「系统拒绝后台 `startForeground()` → 降级成普通通知 → 图标不消失」。
**实测证明这个根因判断是错的**，而且它指向的修法会让我们把「系统状态」和「我们自己的代码状态」混为一谈：

| # | 上一版结论 | 实测结果 | 证据 |
|---|-----------|---------|------|
| ① | 系统拒绝后台 `startForeground()`（`not allowed due to bg restriction`） | **系统从未拒绝过**。两次崩溃前 AM 都已放行 `Background started FGS: Allowed`（08:20 = `code:PACKAGE_REPLACED` 临时许可；09:29 = `code:SYSTEM_ALLOW_LISTED` 永久许可） | §6.2 |
| ② | 用 `nm.activeNotifications` 复核「是不是真被拒了」 | **假阴性**：startForeground 确实成功、dumpsys 里 id=1 的 `flags=0x62` 含 `FLAG_FOREGROUND_SERVICE`，但 `activeNotifications` 查不到 → 误判成「被拒」→ 白白多发一条 id=2 → 状态栏出现**两个** P 图标 | §6.5④ |
| ③ | MIUI 上精确闹钟拉不起 FGS（"MIUI 比 AOSP 更严"） | **闹钟链本身可用**。真正断链的是①的崩溃 + ②的 deviceidle 挂起（`Pending user blocked background alarms`） | §6.3 |

**真正的直接原因：是我们自己的代码跳过了 `startForeground()`，被系统按契约杀进程。**

### 6.1 现象

覆盖安装后（**不打开 App**）：进程在、**没有任何 NotificationRecord**、状态栏图标**整体消失** →
「图标亮 = 已连接」这条产品语义在后台彻底失效；必须从前台 UI 启动（MainActivity ④ 按钮）才有通知。
开机自启链与覆盖升级链受同一限制。

### 6.2 L1（致命 · 代码自造）：跳过 `startForeground()` → 系统按契约杀进程

**v1 代码**：用持久化 prefs `fgs_denied` 决定"是否调用 `startForeground()`"；一旦置 true 就**永久跳过**。
但 `startForegroundService()` 已经调用过了 → 系统等 30 秒后按契约抛异常杀进程：

```
08:20:15  拉起 SenderService（startForegroundService 成功）
08:20:45  I ActivityManager: Bringing down service while still waiting for start foreground
          → RemoteServiceException$ForegroundServiceDidNotStartInTimeException  → 杀进程
08:34:29  再次拉起（此时已加 deviceidle 白名单）→ 08:34:59 同样崩溃
```

**关键反证（推翻"系统拒绝"）**：两次崩溃前 AM 都已放行后台 FGS：

```
08:20:xx I ActivityManager: Background started FGS: Allowed [... uidState: RCVR; code:PACKAGE_REPLACED;
                tempAllowListReason:<broadcast:1000:android.intent.action.MY_PACKAGE_REPLACED...>]
09:29:22 I ActivityManager: Background started FGS: Allowed [... code:SYSTEM_ALLOW_LISTED;
                tempAllowListReason:<,reasonCode:SYSTEM_ALLOW_LISTED,duration:9223372036854775807,...>]
```

⇒ 系统从头到尾**没有拒绝过**，是我们自己没调 `startForeground()`。
⇒ 进程一死，通知随之消失，**状态栏图标整体消失**。

### 6.3 L2（MIUI 设备 idle）：精确闹钟被挂起

```
08:25:20  精确闹钟未触发
dumpsys alarm → "Pending user blocked background alarms:" 下挂着 photobeam 的 AlarmReceiver
```

执行 `adb shell dumpsys deviceidle whitelist +com.photobeam.app` 后：
该区块**立即清空**（`none`）→ 被挂起的闹钟随即触发 → AM 放行 `code:SYSTEM_ALLOW_LISTED`（**永久**许可）。

⇒ MIUI 上决定性的开关是**电池优化白名单（deviceidle whitelist）**，不是「精确闹钟能不能拉起 FGS」。

### 6.4 L3（降级态被回收）

非 FGS 态约 **75 秒**后被 `Stopping service due to app idle` 停掉（Android 8+ 后台服务无法长存）。
⇒ 降级成普通通知只能当**兜底**，不能当最终方案：必须走「真 FGS」。

### 6.5 修复（6 项，全部实机复验）

| # | 改动 | 文件 |
|---|------|------|
| ① | **必调 `startForeground()`**：`postStatus()` 无条件调用（try/catch 兜异常）；`fgsDenied` **降级为纯 UI 提示**，绝不再用于「跳过 startForeground」 | `SenderService.kt` |
| ② | **通知 ID 三分离**：`NOTI_ID=1`（FGS）/ `NOTI_ID_FALLBACK=2`（降级普通通知）/ `NOTI_ID_DOWN=3`（服务未运行提示，走**新通道** `CH_DOWN`，`IMPORTANCE_MIN` → **不占状态栏**）；`onDestroy` 同时 cancel id=1 与 id=2 | `SenderService.kt` |
| ③ | **降级通知带 `setTimeoutAfter(120000)`**：普通通知**不随进程死亡消失**，不设超时会留下「以为还连着」的假图标 | `SenderService.kt` |
| ④ | **判据简化**：抛异常 = 真被拒；不抛 = 成功。**废弃 `activeNotifications` 复核**（实测假阴性 → 会多发一条 id=2 → 双 P 图标） | `SenderService.kt` |
| ⑤ | **自起链改造**：`BootReceiver` / `UpgradeReceiver` 改为**广播里直接起服务** + 精确闹钟兜底（不再只挂闹钟等它响）；`SenderService.start()` 三级降级 FGS → 普通后台服务 → 故障提示 | `SenderService.kt` |
| ⑥ | **后台常驻状态如实显示**：App 内新增「后台常驻：已放行/未放行」状态行（`PowerManager.isIgnoringBatteryOptimizations`）；④ 按钮按优先级依次尝试 4 个 intent（`ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` → MIUI `OP_AUTO_START` → `ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS` → 应用详情页），全失败给手动路径 | `MainActivity.kt` |

> `AndroidManifest.xml` 本轮**无需新增声明**：`SCHEDULE_EXACT_ALARM`(≤32) / `USE_EXACT_ALARM`(33+) / `REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` / `RECEIVE_BOOT_COMPLETED` / `SenderService foregroundServiceType="dataSync|specialUse"` / `UpgradeReceiver` 监听 `MY_PACKAGE_REPLACED` 早已就位。

### 6.6 实机复验（全部通过）

**① 覆盖安装后不打开 App → 服务自起、图标亮、零崩溃**

```
# 覆盖安装触发真实的 MY_PACKAGE_REPLACED（App 未打开）
I ActivityManager: Background started FGS: Allowed [... code:SYSTEM_ALLOW_LISTED ...]
D PhotoBeam: MY_PACKAGE_REPLACED → 自起服务
D PhotoBeam: 状态通知已发 mode=fgs（正规前台服务）
```

- 进程 27309 自起；logcat **零** `ForegroundServiceDidNotStartInTime` / `Bringing down service`。
- dumpsys 通知**只有一条** id=1（`flags=0x62` = `FLAG_FOREGROUND_SERVICE|FLAG_ONGOING_EVENT|FLAG_GROUP_CONVERSATION`），**无残留 id=2**。
- 精确闹钟正常：`origWhen=09:34:27`；`Pending user blocked background alarms: none`。
- 状态栏截图 `32_fgs_v2_statusbar_crop.png`：PhotoBeam 图标位于 **x=130..141**，实测**纯白 (255,255,255)**（最高灰度和 765）→ 亮白空心 P 可见（放大图 `32_p_on_zoom.png`）。

**② 后台自起后的端到端同步（新 seed 3 张内容互异图）**

```
D PhotoBeam: 对账新增 3
D PhotoBeam: 冲刷队列: 待发 3 张 → <PC-NAME>
D PhotoBeam: 落盘确认: PhotoBeam_s2026110909_001_...jpg   （×3）
```

PC 侧 `%LOCALAPPDATA%\PhotoBeam\Received\` 落盘，**字节数与手机完全一致**：

| 文件 | 字节 |
|------|------|
| `1793583396710_PhotoBeam_s2026110909_001_2026-11-02_06-45-00-000.jpg` | 1,854,050 |
| `1793583396817_PhotoBeam_s2026110909_002_2026-11-02_06-45-00-000.jpg` | 1,843,743 |
| `1793583396942_PhotoBeam_s2026110909_003_2026-11-02_06-45-00-000.jpg` | 1,858,001 |

**③ 反向验证：PC 停掉 → 图标变暗但服务不倒**

```
D PhotoBeam: 连通探测失败: failed to connect to /192.168.0.<PHONE-IP> (port 5416) from /192.168.0.<PC-IP> ...
D PhotoBeam: 状态通知已发 mode=fgs（正规前台服务）
```

- 通知通道自动切到 `photobeam_fg_off`；同一位置（x=130..141）P 变**暗**（最高灰度和 765 → **592**，实测 (178,194,219)）→ 截图 `33_fgs_v2_off_icon_crop.png` / `33_p_off_zoom.png`。
- NotificationRecord 仍在、服务仍存活（不崩溃、不被回收）。

**④ 闭环：PC 回来 → P 立刻变亮（语义双向闭环）**

```
09:53:05  PC 端重启 dist\PhotoBeam.exe（PID 15920，TCP :5416 / UDP 发现应答 :5416）
09:53:54  手机端下一轮对账 → 直连探测成功
          D PhotoBeam: 状态通知已发 mode=fgs（正规前台服务）
          dumpsys: NotificationRecord(... id=1 ... channel=photobeam_fg_on_v2 ... flags=0x62)
```

- 通道从 `photobeam_fg_off` **自动切回** `photobeam_fg_on_v2`（仍是 id=1，无新增通知）。
- 状态栏同一位置（x=130..141）P 恢复**纯白 (255,255,255)**，最高灰度和 765 → 截图 `34_fgs_v2_reconnect_statusbar.png` / `34_fgs_v2_reconnect_crop.png` / `34_p_reconnect_zoom.png`。

⇒ 「亮 ⟺ 已连接 / 暗 ⟺ 未连接」在**两个方向**都实测闭环：断开会变暗、恢复会变亮，**全程服务不倒、图标不消失**。

### 6.7 语义定稿：图标 ⟺ 连接

| 状态栏呈现 | 含义 | 通道 / 通知 |
|-----------|------|------------|
| **亮白空心 P**（x=130..141，纯白 255/255/255） | **已连接，同步可工作** | `photobeam_fg_on_v2`，id=1，`flags=0x62`（含 `FLAG_FOREGROUND_SERVICE`） |
| **暗空心 P**（同一位置，灰度 592） | 服务在，但**未连接**（PC 未开 / 网络不通）；队列保留，恢复后自动补发 | `photobeam_fg_off`，id=1 |
| 无图标 | 服务**未运行**（进程确实死了） | 仅下拉可见 `photobeam_fg_down`（`IMPORTANCE_MIN`，**不占状态栏**）说明「后台同步服务未能启动」 |

⇒ 用户不会再遇到「图标没了、但服务其实在跑」（这是 L1 崩溃导致的，已修），
也不会遇到「图标亮着、其实连不上」（连接状态由直连 TCP 轻探测如实驱动，亮/暗区分）。
⇒ **服务运行期间状态栏图标恒可见**，且其亮度**严格对应**同步链路的可用性。

> 仍需用户一次手动授权：MIUI 上必须加入**电池优化白名单**（App 内 ④ 按钮引导，状态行如实显示是否已放行）。
> 未放行时：覆盖安装链仍可自起（`PACKAGE_REPLACED` 临时许可足够），但开机链可能因闹钟被挂起而延迟到心跳周期。

---

## 7. 遗留与建议

1. **后台自启 FGS**（§6）已从根上修好：不再跳过 `startForeground()`、不再出现 30 秒 `ForegroundServiceDidNotStartInTimeException` 崩溃、覆盖安装/开机链可自起、状态栏图标恒可见且亮/暗如实对应连接状态。**仍需用户一次手动授权**（MIUI 电池优化白名单），App 内已做 ④ 引导 + 状态如实显示；未放行时开机链可能延迟到心跳周期。
2. **PC 端 SAS 只落日志、不弹 Receiver 向导窗**（协议 §3.2 要求向导窗）——上一阶段遗留，不在本轮范围。
3. README 未写「覆盖升级需同签名」须知。
4. `pc\` 目录残留旧 exe 等历史遗留物，待清理（本轮已清理一轮，见 §10）。
5. **§6 上一版的结论已被实测推翻**（系统拒绝 `startForeground` / `activeNotifications` 复核 / MIUI 精确闹钟不可用）——文档已更正，下一阶段请以 §6 为准，勿照旧结论回退代码。

---

## 8. 本轮改动文件清单

### 产品代码
| 文件 | 改动 |
|------|------|
| `pc\popup_windows.go` | 缩略图改 `CreateDIBSection + StretchBlt`；`showPopupEx` 单一入口；DrawText flags → `0x00008024`；`PAINTSTRUCT` 72→80 字节；新增缩略图诊断日志；删 `encoding/binary` |
| `android\app\src\main\kotlin\com\photobeam\app\SenderService.kt` | 立即刷第 0 帧；`UPLOAD_FRAMES` 4 个独立 drawable 逐帧切换；MediaStore 250ms 去抖合并；**FGS 后台自起修复（v2，见 §6.5）**：`postStatus()` **无条件 `startForeground()`**（`fgsDenied` 降为纯 UI 提示，不再用于跳过）；通知 ID 三分离（1=FGS / 2=降级 / 3=故障提示）+ 降级通知 `setTimeoutAfter(120s)` + 新通道 `CH_DOWN`(IMPORTANCE_MIN)；**废弃 `activeNotifications` 复核**；`start()` 三级降级（FGS→普通服务→故障提示）；`BootReceiver`/`UpgradeReceiver` 广播内**直接起服务** + 精确闹钟兜底；精确闹钟链路（`scheduleAlarmAt`/`scheduleHeartbeatChain`）；闹钟链去重（不再被 30s reconcile 推后）；`onDestroy` 主动取消 id=1 + id=2 |
| `android\app\src\main\kotlin\com\photobeam\app\MainActivity.kt` | `onResume` 自动发 `ACTION_ENSURE_FGS`；状态栏图标模式如实显示；新增 ④「允许后台常驻（关闭电池优化）」；**新增「后台常驻：已放行/未放行」状态行**（`isIgnoringBatteryOptimizations()`）；④ 按钮按优先级依次尝试 4 个 intent（`REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` → MIUI `OP_AUTO_START` → `IGNORE_BATTERY_OPTIMIZATION_SETTINGS` → 应用详情页），全失败给手动路径 |
| `android\app\src\main\AndroidManifest.xml` | 新增 `SCHEDULE_EXACT_ALARM`（≤API32）/ `USE_EXACT_ALARM`（API33+）；`REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` / `RECEIVE_BOOT_COMPLETED` / `SenderService foregroundServiceType="dataSync|specialUse"` / `UpgradeReceiver` 监听 `MY_PACKAGE_REPLACED`（**§6 本轮无需新增声明**，此前已就位） |
| `pc\icon.ico` | **重生成**：7 个尺寸（16/24/32/48/64/128/256），P 改为**居中**、高占 66%，并剔除母版顶边毛刺 |
| `design\icon_glyph_p.png`（新） | 白色空心 P 字形（唯一真源，宽高比 0.427） |
| `design\icon_shape.png`（新） | 深色圆角方块底图（唯一真源，已剔除毛刺） |
| `design\icon_master_512.png` | 按新版式（P 居中）重生成 |
| `design\icon_preview.png` | 设计总览重生成 |
| `android\app\src\main\res\mipmap-{m,h,xh,xxh,xxxh}dpi\ic_launcher_foreground.png` | 重生成：P 相对蒙版可见区**居中**，高占可见区 78% |
| `android\app\src\main\res\mipmap-{m,h,xh,xxh,xxxh}dpi\ic_launcher.png` | 重生成：P 居中，高占 66% |

### 新增工具（可复用回归）
| 文件 | 用途 |
|------|------|
| `tools\gen-app-icons.py` | **统一生成器**：母版 / 设计总览 / `pc\icon.ico` / Android 5 密度传统图标 + 自适应前景（幂等可重跑） |
| `tools\extract-icon-parts.py` | 从母版一次性抽出 `icon_glyph_p.png` + `icon_shape.png`（含顶边毛刺裁除） |
| `tools\gen-launcher-icons.py` | v1 起点（把 P 锚定到蒙版可见区），已被 `gen-app-icons.py` 取代 |
| `tools\gen-test-images.py` | 生成 N 张内容互异测试图（避免 PC 幂等去重） |
| `tools\capture-upload-anim.ps1` | 传输动画一键验证：生成→推送→批量定稿→连拍→帧哈希 |
| `tools\analyze-upload-anim.py` | 录屏抽帧 + 波点纵向位置量化（判定 4 帧轮转） |
| `tools\capture-notif-frames.ps1` | 状态栏图标帧哈希对比（判静止/在动） |
| `tools\capture-tray-icon.ps1` | 托盘图标 UIA 定位 + tooltip 佐证抓图 |
| `tools\capture-popup.ps1` | PC 弹窗抓图 |
| `tools\watch-popup-window.ps1` | 弹窗窗口监视诊断 |
| `pc\dibtest3\main.go` | `StretchDIBits` vs `CreateDIBSection+StretchBlt` 对照实验（根因证据） |

---

## 9. 证据文件索引（`docs\stage4-screenshots\`）

| 文件 | 内容 |
|------|------|
| `04_pc_popup_thumb.png` | PC 弹窗：左侧缩略图 + 文件名省略号（验收点 1、2） |
| `18_upload_anim_levels.png` | 传输动画 5 档波点位置并排（验收点 3） |
| `anim9_video\anim_timeline.txt` | 逐帧状态台账（IDLE-P ↔ L1..L5） |
| `anim9_video\anim_levels_summary.png` | 同上，横向汇总图 |
| `pb_anim9.mp4` | 设备端原始录屏（可复算） |
| `26_launcher_icon_v2.png` | v2 修复后的实机桌面图标（深色方块 + 居中放大的空心 P，验收点 4） |
| `29_phone_home_final.png` | 交付态手机桌面全屏实拍（PhotoBeam 图标在右下角，状态栏 P 可见） |
| `30_fallback_icon.png` | **旧版（降级路径）证据**，已被 §6 的 v2 方案取代：覆盖安装后**不打开 App**，状态栏仍是空心 P |
| `31_fallback_statusbar.png` | 同上，状态栏左半段 3× 放大（静音铃 + 空心 P + 第三方方块图标） |
| `32_fgs_v2_statusbar.png` | **§6 v2 核心证据**：覆盖安装后**不打开 App**，进程自起、**真 FGS**（dumpsys 单条 id=1 / flags=0x62），状态栏亮白 P |
| `32_fgs_v2_statusbar_crop.png` | 同上，状态栏左段裁切（1080×110）：PhotoBeam P 位于 **x=130..141**，实测**纯白 (255,255,255)** |
| `32_p_on_zoom.png` | 亮白空心 P 放大图（384×432） |
| `33_fgs_v2_off_icon.png` | **反向验证**：PC 停掉后，通道自动切 `photobeam_fg_off`，P **变暗但仍在**，服务存活 |
| `33_fgs_v2_off_icon_crop.png` | 同上，状态栏左段裁切：同一位置（x=130..141）P 最高灰度和 765 → **592**（(178,194,219)） |
| `33_p_off_zoom.png` | 暗 P 放大图（384×432） |
| `34_fgs_v2_reconnect_statusbar.png` | **闭环证据**：PC 端重启后手机端自动恢复连接，状态栏 P 恢复纯白 |
| `34_fgs_v2_reconnect_crop.png` | 同上，状态栏左段裁切（x=130..141 实测纯白 255/255/255） |
| `34_p_reconnect_zoom.png` | 重连后的亮白 P 放大图 |
| `probeimgs_fgs2\` | §6 端到端复验用的 3 张**内容互异**测试图（新 seed，PC 侧 SHA-256 不去重） |
| `13_launcher_icon_zoom.png` | 修复前的实机启动器图标（缺陷态对照） |
| `22_three_icons_consistent.png` | 桌面 / 通知 / 托盘三处图标一致对比（v2 实拍） |
| `21_notif_icon_zoom.png` | 状态栏空心 P（空闲态，实机状态栏 x=280..290 裁切） |
| `28_pc_tray_icon_v2_zoom.png` | v2 修复后的 PC 托盘图标（12× 放大，P 完全居中） |
| `27_pc_tray_icon_v2_panel.png` | 托盘溢出面板实拍（含 tooltip 身份佐证） |
| `25_icons_v2_layout.png` | v2 版式生成器合成预览（启动器/传统/托盘 16/32/母版） |
| `10_pc_tray_icon.png` | 托盘区域实拍（v1，含 tooltip 身份佐证） |
| `16_stat_icons_strip.png` | 5 个状态栏图标资源（`ic_stat_p` / `ic_stat_up_0..3`） |
| `17_anim6_unique_frames.png` | 传输期状态栏 5 个唯一帧 |
| `frames\idle3\` | 空闲 16 帧原始截图 + `frames.csv`（STATIC 判定，16/16 哈希全同） |
| `frames\anim6\` | 首次状态栏连拍 45 帧的**帧哈希台账** `frames.csv` + 唯一帧放大图 `icons\` + 实机抽屉帧 `f40.png`（原始帧已按体积裁剪，逐帧哈希可复算） |

---

## 10. 本轮清理（均走可恢复通道：本地 `mavis-trash` / 设备 `adb shell rm`）

| 对象 | 处理 |
|------|------|
| `pc\dibtest\`、`pc\dibtest2\`（早期对照实验，已被 dibtest3 取代） | 已删 |
| `pc\PhotoBeam.exe`（旧残留 exe） | 已删 |
| `android\build\s5_console.exe`、`s5_gui.exe`、`anim\`、`instchk\`（上轮遗留） | 已删 |
| `docs\stage4-screenshots\genimgs_anim4..9\`（生成的测试图，424 MB） | 已删 |
| `docs\stage4-screenshots\probeimgs\`（164 张探测图） | 已删 |
| `docs\stage4-screenshots\anim8_video\`、`pb_anim8.mp4`（录屏落在锁屏的那次，分析无效） | 已删 |
| `docs\stage4-screenshots\frames\busy*`、`idle`、`idle2`、空的 `anim4/5/7/8/9` | 已删 |
| `docs\stage4-screenshots\_*.png`（22 张过程截图）、`19_launcher_drawer.png`（误截到第三方 App） | 已删 |
| `docs\stage4-screenshots\frames\anim6\` 冗余原始帧（保留 `frames.csv` + `icons\` + `f40.png`） | 已删 44 帧 |
| 设备 `/sdcard/Pictures/PhotoBeam_*`（169 张测试图）与 `/sdcard/Pictures/pb_anim*`（5 个目录） | 已删 |
| 设备 `/data/local/tmp/t.raw`、`/data/local/tmp/anim`、`/data/local/tmp/pb_burst` | 已删 |
| v1 图标证据 `20_launcher_icon_fixed.png`、`23_pc_tray_icon_only.png`、`15_icon_fix_preview.png`（已被 v2 取代） | 已删 |
| `tools\gen-launcher-icons.py`（v1 生成器，已被 `gen-app-icons.py` 取代） | 已删 |
| **§6 本轮**：根目录 `_sb3.py`、`_sb4.py`（状态栏像素分析临时脚本） | 已删 |
| **§6 本轮**：`docs\stage4-screenshots\_sb_off.png`、`_sb_v3.png`、`_sb_v4.png`、`_h_bottom.png`（过程截图） | 已删 |
| **§6 本轮**：`docs\stage4-screenshots\icon_poll.{err,log,txt}`、`popup_capture.{err,log}`、`watch_windows.{err,log}`（工具诊断日志，无证据引用） | 已删 |
| **§6 本轮**：`.tmp_aosp_alarm.java`（AOSP 源码下载 404 产物） | 本轮确认**已不存在**，无残留 |

**保留**：`pc\dibtest3\`（StretchDIBits vs StretchBlt 对照实验，是 §1.1 根因结论的可复算证据）、设备用户的 `<USER-DIR>` / `<USER-DIR>` 目录、所有编号证据文件、`frames\idle3\` 与 `frames\anim6\frames.csv`+`icons\`。

**本轮刻意未删**（如实说明理由）：

- 根目录 `delivery_*.png` / `v_*.png` / `nodialog_check.png` / `pc_sas.png` / `enum_tray.py` → 被 `docs\stage2-evidence.md`、`stage3-evidence.md`、`.lam\` 交接件引用，是**前序阶段证据**，不是本轮产物。
- 设备 `/sdcard/Pictures/<USER-DIR>/`、`<USER-DIR>B/`（本轮端到端测试图各 3 张）→ 删掉后 MediaStore 仍会列出这些条目，App 对账会重新入队但文件已不存在 → 队列留下永久「文件未就绪」的 pending 项，污染运行状态。

---

## 11. 复现要点

```powershell
# 1) 构建
cmd /c "cd /d C:\Users\<user>\Project\PhotoBeam && build-android.bat"
cmd /c "cd /d C:\Users\<user>\Project\PhotoBeam && build-pc.bat"

# 2) 装机（同签名覆盖安装，配对与数据保留）
adb install -r PhotoBeam.apk
#    ⚠ App 必须**至少启动过一次**，否则 stopped=true → MY_PACKAGE_REPLACED 广播不送达
#      （adb shell am force-stop 也会置 stopped=true）

# 3) 覆盖安装自起（**不需要打开 App**，这是 §6 的验收点）
#    安装后等 3~5 秒，手机日志应出现：
#      D PhotoBeam: MY_PACKAGE_REPLACED → 自起服务
#      D PhotoBeam: 状态通知已发 mode=fgs（正规前台服务）
adb logcat -d -s PhotoBeam | Select-String 'mode=fgs'

# 3b) MIUI 一次性授权（否则精确闹钟会被 deviceidle 挂起）
adb shell dumpsys deviceidle whitelist +com.photobeam.app
adb shell dumpsys deviceidle whitelist | Select-String photobeam   # user,com.photobeam.app,10266
adb shell dumpsys alarm | Select-String 'Pending user blocked' -Context 0,3   # 应为 none

# 3c) 核对通知（应**只有 id=1 一条**，flags=0x62 含 FLAG_FOREGROUND_SERVICE）
adb shell dumpsys notification --noredact | Select-String 'NotificationRecord\(.*pkg=com\.photobeam'

# 4) 端到端同步（后台状态下也要跑通）
python tools\gen-test-images.py docs\stage4-screenshots\probeimgs_fgs2 3 1080 2340 92 2026110909
adb push docs\stage4-screenshots\probeimgs_fgs2 /sdcard/Pictures/<USER-DIR>B/
adb shell content call --uri content://media/external/images/media --method scan_volume --arg external_primary
#    等 ~30s 对账（MediaStore 扫描入库有延迟），手机日志应出现
#      「对账新增 3」「冲刷队列: 待发 3 张 → <PC-NAME>」+ 3 次「落盘确认」
#    PC 侧落盘：%LOCALAPPDATA%\PhotoBeam\Received\

# 5) 传输动画验证（一键）
pwsh -File tools\capture-upload-anim.ps1 -Count 40 -Tag animX -Seed <新种子>
#    若要 4 帧轮转的量化证据：录屏 + 抽帧
adb shell screenrecord --time-limit 50 --bit-rate 16000000 --size 1080x2400 /data/local/tmp/pb_anim.mp4
python tools\analyze-upload-anim.py <mp4> <outDir>
```

> 测试图必须**内容互异**且**文件名每批唯一**，否则分别会被 PC 幂等去重、或被 MediaStore 复用旧 `_id` 而漏收（见 §3.5）。
> §6 的关键复现点：**不要打开 App**——只有这样才能真正验证「覆盖安装后后台自起 + 图标常驻 + 同步可用」。

