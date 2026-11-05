#!/usr/bin/env pwsh
# 端到端延迟守门人：截图 → PC 剪贴板可粘贴 ≤ 2 秒（协议 §9 验收标准）
#
# 原理：在 PC 上轮询 CF_DIB 是否就绪（CF_DIB 一出现 = 用户已经可以 Ctrl+V），
#       同时用 adb 触发一次系统截图，两者的时差就是端到端延迟。
#
# 用法：
#   pwsh -File tools/assert-latency.ps1                    # 默认 3 轮，阈值 2000ms
#   pwsh -File tools/assert-latency.ps1 -Rounds 5          # 多跑几轮
#   pwsh -File tools/assert-latency.ps1 -ThresholdMs 2500  # 放宽阈值
#
# 前置：PC 端 PhotoBeam.exe 在跑；手机已配对；adb 设备在线。
# 退出码：0 = 全部达标；1 = 有轮次超标。

param(
  [int]$Rounds = 3,
  [int]$ThresholdMs = 2000,
  [string]$Serial = ""
)

$ErrorActionPreference = 'Continue'

# ---------- 探针：C# 编译，轮询 CF_DIB ----------
$csc = 'C:\WINDOWS\Microsoft.NET\Framework64\v4.0.30319\csc.exe'
if (-not (Test-Path $csc)) {
  Write-Error "找不到 csc.exe：$csc"
  exit 1
}

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
  [DllImport("user32.dll")] static extern bool OpenClipboard(IntPtr h);
  [DllImport("user32.dll")] static extern bool EmptyClipboard();
  [DllImport("user32.dll")] static extern bool CloseClipboard();

  static int Main(string[] args) {
    int threshold = args.Length > 0 ? int.Parse(args[0]) : 2000;
    string adbArgs = args.Length > 1 ? args[1] : "";

    // 清空剪贴板，让探针起点干净（副作用：当前剪贴板内容丢失）
    if (OpenClipboard(IntPtr.Zero)) { EmptyClipboard(); CloseClipboard(); }
    Thread.Sleep(300);

    var sw = Stopwatch.StartNew();
    var t = new Thread(() => {
      while (sw.ElapsedMilliseconds < threshold * 3) {
        if (IsClipboardFormatAvailable(8)) break;   // CF_DIB
        Thread.Sleep(1);
      }
    });
    t.IsBackground = true;
    t.Start();

    Thread.Sleep(600);   // 等探针线程起跑

    var psi = new ProcessStartInfo("adb");
    foreach (var a in adbArgs.Split('|')) if (a.Length > 0) psi.ArgumentList.Add(a);
    psi.UseShellExecute = false;
    psi.CreateNoWindow = true;
    psi.RedirectStandardOutput = true;
    psi.RedirectStandardError = true;
    using (var p = Process.Start(psi)) p.WaitForExit();

    t.Join(threshold * 3);
    if (IsClipboardFormatAvailable(8)) {
      Console.WriteLine("LATENCY_MS=" + sw.ElapsedMilliseconds);
      return 0;
    }
    Console.WriteLine("LATENCY_MS=-1");
    return 1;
  }
}
'@
Set-Content -LiteralPath $src -Value $code -Encoding UTF8
& $csc /nologo /out:$exe $src 2>&1 | Out-Null
if (-not (Test-Path $exe)) {
  Write-Error "探针编译失败"
  exit 1
}

# ---------- 环境检查 ----------
Write-Output "=== 端到端延迟守门人（阈值 ${ThresholdMs}ms，${Rounds} 轮）==="

$pb = Get-Process -Name PhotoBeam -ErrorAction SilentlyContinue
if (-not $pb) {
  Write-Error "PhotoBeam.exe 未运行，先启动接收端"
  exit 1
}
Write-Output ("  PhotoBeam 进程: " + ($pb.Id -join ', '))

$adbArgs = @('shell', 'input', 'keyevent', 'KEYCODE_SYSRQ')
if ($Serial) { $adbArgs = @('-s', $Serial) + $adbArgs }
$probeArgs = ($adbArgs -join '|')

# ---------- 跑 ----------
$results = @()
for ($i = 1; $i -le $Rounds; $i++) {
  $out = & $exe $ThresholdMs $probeArgs 2>&1
  $ms = -1
  foreach ($l in $out) {
    if ($l -match 'LATENCY_MS=(-?\d+)') { $ms = [int]$matches[1] }
  }
  if ($ms -lt 0) {
    Write-Output ("  第 $i 轮: 超时（${ThresholdMs}ms 内 CF_DIB 未就位）")
    $results += [pscustomobject]@{ Round = $i; Ms = -1; Pass = $false }
  } else {
    $pass = $ms -le $ThresholdMs
    $flag = if ($pass) { 'OK  ' } else { 'FAIL' }
    Write-Output ("  第 $i 轮: ${ms}ms  ${flag}")
    $results += [pscustomobject]@{ Round = $i; Ms = $ms; Pass = $pass }
  }
  Start-Sleep -Seconds 2
}

# ---------- 汇总 ----------
$ok = @($results | Where-Object { $_.Pass }).Count
$valid = @($results | Where-Object { $_.Ms -gt 0 })
$avg = 0; $max = 0
if ($valid.Count -gt 0) {
  $avg = [math]::Round(($valid | Measure-Object Ms -Average).Average)
  $max = ($valid | Measure-Object Ms -Maximum).Maximum
}

Write-Output ''
Write-Output "=== 汇总 ==="
Write-Output ("  达标 $ok / $($results.Count) 轮")
if ($valid.Count -gt 0) {
  Write-Output ("  平均 ${avg}ms  最大 ${max}ms  阈值 ${ThresholdMs}ms")
}
Write-Output ("  结论: " + $(if ($ok -eq $results.Count) { "通过" } else { "未达标" }))

# 清理
Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue

if ($ok -eq $results.Count) { exit 0 } else { exit 1 }
