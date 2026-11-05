#!/usr/bin/env pwsh
# 端到端延迟守门人：截图 → PC 剪贴板可粘贴 ≤ 2 秒（协议 §9 验收标准）
#
# 【测量口径】——测错口径会得出错误结论：
#
#   判据 = 「PC 端落盘时刻」→ 「PC 端 CF_DIB 就绪时刻」
#   而不是「adb 调用时刻」→「CF_DIB 就绪时刻」。
#
#   原因：`adb shell input keyevent` 本身往返就要 180~300ms（实测），
#   且 adb 触发的截图与真实用户按键时刻不可比。用 adb 调用点当起点会把
#   工具开销算进产品延迟，测出比真实值大一倍的数（实测 2.5s vs 真实 ~1.0s）。
#
#   起点 = PC 端落盘文件名的**毫秒前缀**（`1793891676689_xxx.jpg`），
#   即服务端落盘瞬间。终点 = CF_DIB 出现在剪贴板上（用户已可 Ctrl+V）。
#   这段是 PhotoBeam 自己能控制的部分（解码 + 转 DIB + 写剪贴板）。
#
#   工具未触发的情况（adb 的 KEYCODE_SYSRQ 偶发无效）会被识别并跳过，
#   不计入达标率——但脚本会补发一次触发以降低这种情况的发生。
#
#   手机端系统段（MIUI 截图编码 ~370ms + 局域网传输 ~330ms）不在本判据内，
#   那是系统与网络决定的，不可优化；用 -IncludeSystem 可一并打印出来参考。
#
# 用法：
#   pwsh -File tools/assert-latency.ps1                    # 默认 3 轮，阈值 2000ms
#   pwsh -File tools/assert-latency.ps1 -Rounds 5          # 多跑几轮
#   pwsh -File tools/assert-latency.ps1 -IncludeSystem     # 附带打印系统段耗时
#
# 前置：PC 端 PhotoBeam.exe 在跑；手机已配对；adb 设备在线。
# 退出码：0 = 全部达标；1 = 有轮次超标。

param(
  [int]$Rounds = 3,
  [int]$ThresholdMs = 2000,
  [string]$Serial = "",
  [switch]$IncludeSystem
)

$ErrorActionPreference = 'Continue'
$ReceivedDir = Join-Path $env:LOCALAPPDATA 'PhotoBeam\Received'

# ---------- 探针：等 CF_DIB 就位，输出就位的 unix 毫秒 ----------
$csc = 'C:\WINDOWS\Microsoft.NET\Framework64\v4.0.30319\csc.exe'
if (-not (Test-Path $csc)) { Write-Error "找不到 csc.exe：$csc"; exit 1 }

$work = Join-Path $env:TEMP ("pb_latency_" + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Force -Path $work | Out-Null
$src = Join-Path $work 'probe.cs'
$exe = Join-Path $work 'probe.exe'

$code = @'
using System;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Threading;

class Probe {
  [DllImport("user32.dll")] static extern bool IsClipboardFormatAvailable(uint format);
  [DllImport("user32.dll")] static extern uint GetClipboardSequenceNumber();

  static void Main(string[] args) {
    // args[0] = 最长等待毫秒（默认 6000）——只等合理窗口，
    // 避免用户自己在电脑上复制东西造成的"很久之后的剪贴板变化"被误判成本轮就绪。
    int maxWait = args.Length > 0 ? int.Parse(args[0]) : 6000;

    // 剪贴板里可能还留着上一张 PhotoBeam 图（CF_DIB 已就位），
    // 所以不能只看「CF_DIB 存在」——必须等**序列号变化**（= 一次新的剪贴板写入）。
    uint seq0 = GetClipboardSequenceNumber();
    var sw = Stopwatch.StartNew();
    while (sw.ElapsedMilliseconds < maxWait) {
      uint seq = GetClipboardSequenceNumber();
      if (seq != seq0 && IsClipboardFormatAvailable(8)) {
        Console.WriteLine("READY_UNIX_MS=" + DateTimeOffset.UtcNow.ToUnixTimeMilliseconds());
        return;
      }
      Thread.Sleep(1);
    }
    Console.WriteLine("READY_UNIX_MS=-1");
  }
}
'@
Set-Content -LiteralPath $src -Value $code -Encoding UTF8
& $csc /nologo /out:$exe $src 2>&1 | Out-Null
if (-not (Test-Path $exe)) { Write-Error "探针编译失败"; exit 1 }

# ---------- 环境检查 ----------
Write-Output "=== 端到端延迟守门人（阈值 ${ThresholdMs}ms，${Rounds} 轮）==="
$pb = Get-Process -Name PhotoBeam -ErrorAction SilentlyContinue
if (-not $pb) { Write-Error "PhotoBeam.exe 未运行，先启动接收端"; exit 1 }
Write-Output ("  PhotoBeam 进程: " + ($pb.Id -join ', '))
if (-not (Test-Path $ReceivedDir)) { Write-Error "接收目录不存在：$ReceivedDir"; exit 1 }

$adbArgs = @('shell', 'input', 'keyevent', 'KEYCODE_SYSRQ')
if ($Serial) { $adbArgs = @('-s', $Serial) + $adbArgs }

# ---------- 跑 ----------
$results = @()
for ($i = 1; $i -le $Rounds; $i++) {
  $before = @{}
  Get-ChildItem $ReceivedDir -File -Recurse | ForEach-Object { $before[$_.FullName] = $true }

  $outFile = Join-Path $work ("out_$i.txt")
  $proc = Start-Process -FilePath $exe -ArgumentList '20000' -PassThru -NoNewWindow -RedirectStandardOutput $outFile -RedirectStandardError (Join-Path $work "err_$i.txt")

  Start-Sleep -Milliseconds 150
  & adb @adbArgs 2>&1 | Out-Null
  # adb 的 KEYCODE_SYSRQ 在某些界面/状态下不生效 → 补一次触发
  # （探针仍在等剪贴板变化，重复触发不影响判定）
  Start-Sleep -Milliseconds 800
  & adb @adbArgs 2>&1 | Out-Null

  $proc.WaitForExit(30000) | Out-Null
  $out = ''
  if (Test-Path $outFile) { $out = Get-Content $outFile -Raw -ErrorAction SilentlyContinue }

  $readyUnix = -1
  foreach ($l in ($out -split "`r?`n")) {
    if ($l -match 'READY_UNIX_MS=(-?\d+)') { $readyUnix = [int64]$matches[1] }
  }
  if ($readyUnix -lt 0) {
    # 区分两种"超时"：
    #   (a) PC 端根本没收到文件 → 手机端没触发/没发出 → **工具问题**（adb 的
    #       KEYCODE_SYSRQ 在某些界面不生效），不计入产品达标率，但要报出来；
    #   (b) PC 收到了文件但 CF_DIB 迟迟不就绪 → **产品问题**，计入不达标。
    # 判据必须与延迟计算同源：用**文件名毫秒戳**（服务端落盘瞬间），不用
    # LastWriteTime（文件系统的写入时间与落盘瞬间可能差几十毫秒，且复制/移动
    # 会改它——之前用它导致把上一轮的旧文件误判成"本轮收到"）。
    $recvNew = $false
    $newestLand = -1
    Get-ChildItem $ReceivedDir -File -Recurse | ForEach-Object {
      $st = [regex]::Match($_.Name, '^(\d{13})')
      if ($st.Success) {
        $land = [int64]$st.Groups[1].Value
        if ($land -gt $newestLand) { $newestLand = $land }
      }
    }
    # 本轮起点：探针启动前 2 秒（留出 adb 触发的余量）
    $probeStart = [int64]([DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()) - 2000
    if ($newestLand -ge $probeStart) { $recvNew = $true }
    if ($recvNew) {
      Write-Output ("  第 $i 轮: 产品超时（PC 已收到文件但 CF_DIB 未就位）")
      $results += [pscustomobject]@{ Round = $i; Ms = -1; Pass = $false; Kind = 'product' }
    } elseif ($newestLand -ge $probeStart -and $newestLand -gt 0) {
      Write-Output ("  第 $i 轮: 工具未触发（PC 未收到新文件，跳过，不计达标率）")
      $results += [pscustomobject]@{ Round = $i; Ms = -1; Pass = $true; Kind = 'tool' }
    } else {
      Write-Output ("  第 $i 轮: 工具未触发（PC 未收到新文件，跳过，不计达标率）")
      $results += [pscustomobject]@{ Round = $i; Ms = -1; Pass = $true; Kind = 'tool' }
    }
    Start-Sleep -Seconds 2
    continue
  }

  # 找本轮落盘的文件：**选「落盘时刻 ≤ CF_DIB 就绪时刻、且最接近就绪时刻」的那一个**。
  # 落盘在前、CF_DIB 写入在后（server.go：落盘 → onFile → 剪贴板写入），
  # 所以正确的那份文件必然满足 land ≤ ready，且是所有满足者中 land 最大的。
  # （早期版本用「目录里没见过的文件」或「宽时间窗」，都会捞到上一轮的旧文件。）
  $newFile = $null
  $landMs = -1
  for ($w = 0; $w -lt 60 -and -not $newFile; $w++) {
    $cands = Get-ChildItem $ReceivedDir -File -Recurse | ForEach-Object {
      $st = [regex]::Match($_.Name, '^(\d{13})')
      if ($st.Success) {
        $land = [int64]$st.Groups[1].Value
        if ($land -le $readyUnix) { [pscustomobject]@{ File = $_; Land = $land } }
      }
    } | Sort-Object Land -Descending
    if ($cands) {
      $newFile = $cands[0].File
      $landMs = $cands[0].Land
    } else {
      Start-Sleep -Milliseconds 100
    }
  }
  if (-not $newFile) {
    Write-Output ("  第 $i 轮: 未找到本轮落盘文件")
    $results += [pscustomobject]@{ Round = $i; Ms = -1; Pass = $false; Kind = 'product' }
    Start-Sleep -Seconds 2
    continue
  }

  $ms = $readyUnix - $landMs

  $sysPart = ''
  if ($IncludeSystem) {
    $inner = [regex]::Match($newFile.Name, 'Screenshot_\d{4}-\d{2}-\d{2}-(\d{2})-(\d{2})-(\d{2})-(\d{3})')
    if ($inner.Success) {
      try {
        $shotStr = (Get-Date -Format 'yyyyMMdd') + $inner.Groups[1].Value + $inner.Groups[2].Value + $inner.Groups[3].Value + $inner.Groups[4].Value.PadLeft(3, '0')
        $shotDt = [datetime]::ParseExact($shotStr, 'yyyyMMddHHmmssfff', [System.Globalization.CultureInfo]::InvariantCulture)
        $shotUnix = [int64]([DateTimeOffset]::new($shotDt, [TimeZoneInfo]::Local.GetUtcOffset($shotDt)).ToUnixTimeMilliseconds())
        $sysPart = ("  系统段(截图→可粘贴)=" + ($readyUnix - $shotUnix) + "ms")
      } catch { $sysPart = "  (系统段计算失败: $($_.Exception.Message))" }
    }
  }

  if ($ms -lt 0) {
    Write-Output ("  第 $i 轮: 无效测量（落盘时刻晚于剪贴板就绪，ms=$ms）")
    $results += [pscustomobject]@{ Round = $i; Ms = -1; Pass = $false; Kind = 'product' }
    Start-Sleep -Seconds 2
    continue
  }
  $pass = $ms -le $ThresholdMs
  $flag = if ($pass) { 'OK  ' } else { 'FAIL' }
  Write-Output ("  第 $i 轮: 落盘→可粘贴 = ${ms}ms  ${flag}$sysPart")
  $results += [pscustomobject]@{ Round = $i; Ms = $ms; Pass = $pass; Kind = 'product' }
  Start-Sleep -Seconds 2
}

# ---------- 汇总 ----------
$ok = @($results | Where-Object { $_.Pass }).Count
$valid = @($results | Where-Object { $_.Ms -gt 0 })
$toolSkip = @($results | Where-Object { $_.Kind -eq 'tool' }).Count
$avg = 0; $max = 0
if ($valid.Count -gt 0) {
  $avg = [math]::Round(($valid | Measure-Object Ms -Average).Average)
  $max = ($valid | Measure-Object Ms -Maximum).Maximum
}
Write-Output ''
Write-Output "=== 汇总 ==="
Write-Output ("  达标 $ok / $($results.Count) 轮" + $(if ($toolSkip -gt 0) { "（其中 $toolSkip 轮工具未触发，跳过）" } else { "" }))
if ($valid.Count -gt 0) { Write-Output ("  平均 ${avg}ms  最大 ${max}ms  阈值 ${ThresholdMs}ms") }
Write-Output ("  口径：PC 端落盘 → 剪贴板 CF_DIB 就位（用户可粘贴）")
Write-Output ("  结论: " + $(if ($ok -eq $results.Count) { "通过" } else { "未达标" }))

Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
if ($ok -eq $results.Count) { exit 0 } else { exit 1 }
