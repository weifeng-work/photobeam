<#
  PhotoBeam 弹窗抓图工具（可复用回归）
  用法：powershell -File tools\capture-popup.ps1 -Out <输出png> [-Class PhotoBeamPopup] [-TimeoutSec 20] [-Wait]
  原理：轮询枚举顶层窗口，找到目标类名后按窗口 rect 从屏幕 DC 做 BitBlt 截图。
  弹窗是 WS_POPUP|TOPMOST，直接抓屏幕即可拿到真实渲染结果（含缩略图与省略号）。
#>
param(
  # 默认输出 = <项目根>\popup_capture.png（随脚本位置推导，不写死本机路径）
  [string]$Out = "",
  [string]$Class = "PhotoBeamPopup",
  [int]$TimeoutSec = 20,
  [switch]$NoWait      # 默认行为=轮询等待窗口出现；加 -NoWait 只查一次
)

# 项目根 = 本脚本所在 tools\ 的上一级
$proj = Split-Path -Parent $PSScriptRoot
if (-not $Out) { $Out = Join-Path $proj "popup_capture.png" }

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing

if (-not ("Win32Cap" -as [type])) {
Add-Type @"
using System;
using System.Text;
using System.Runtime.InteropServices;
public class Win32Cap {
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern IntPtr GetDC(IntPtr h);
  [DllImport("user32.dll")] public static extern int ReleaseDC(IntPtr h, IntPtr dc);
  [DllImport("gdi32.dll")] public static extern bool BitBlt(IntPtr dst, int x, int y, int w, int h, IntPtr src, int sx, int sy, int rop);
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  public static IntPtr Find(string cls) {
    IntPtr found = IntPtr.Zero;
    EnumWindows((h, l) => {
      var sb = new StringBuilder(256); GetClassName(h, sb, 256);
      if (sb.ToString() == cls && IsWindowVisible(h)) { found = h; return false; }
      return true;
    }, IntPtr.Zero);
    return found;
  }
}
"@
}

$deadline = (Get-Date).AddSeconds($TimeoutSec)
$hwnd = [IntPtr]::Zero
while ((Get-Date) -lt $deadline) {
  $hwnd = [Win32Cap]::Find($Class)
  if ($hwnd -ne [IntPtr]::Zero) { break }
  if ($NoWait) { break }
  Start-Sleep -Milliseconds 120
}
if ($hwnd -eq [IntPtr]::Zero) {
  Write-Output "NOT_FOUND: 没有找到可见窗口类 '$Class'（等待 ${TimeoutSec}s）"
  exit 2
}

$r = New-Object Win32Cap+RECT
[void][Win32Cap]::GetWindowRect($hwnd, [ref]$r)
$w = $r.Right - $r.Left; $h = $r.Bottom - $r.Top
Write-Output "FOUND hwnd=$hwnd rect=($($r.Left),$($r.Top))-${w}x${h}"
# 等一拍再截：确保 WM_PAINT 已完成（缩略图 StretchDIBits 与文字都已画上）
Start-Sleep -Milliseconds 300

$srcDC = [Win32Cap]::GetDC([IntPtr]::Zero)
$dstDC = [IntPtr]::Zero
try {
  $bmp = New-Object System.Drawing.Bitmap $w, $h
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $dstDC = $g.GetHdc()
  # SRCCOPY = 0x00CC0020
  [void][Win32Cap]::BitBlt($dstDC, 0, 0, $w, $h, $srcDC, $r.Left, $r.Top, 0x00CC0020)
  $g.ReleaseHdc($dstDC); $dstDC = [IntPtr]::Zero
  $g.Dispose()
  $bmp.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
  $bmp.Dispose()
  Write-Output "SAVED $Out"
} finally {
  if ($dstDC -ne [IntPtr]::Zero) { $g.ReleaseHdc($dstDC) }
  [void][Win32Cap]::ReleaseDC([IntPtr]::Zero, $srcDC)
}
