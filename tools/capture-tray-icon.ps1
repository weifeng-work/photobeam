<#
  PhotoBeam 托盘图标抓图（UIA 定位 + tooltip 佐证）
  用法：powershell -File tools\capture-tray-icon.ps1 -Out <png> [-NameMatch "PhotoBeam"]

  流程：
  1) 真实鼠标点击任务栏「显示隐藏的图标」chevron，打开通知区域溢出面板
     （已是打开态时先点一次关闭，再点一次打开——面板状态不干净会枚举不到图标）；
  2) 用 UIAutomation 在面板里按名称找目标托盘项（本项目=PhotoBeam 接收器 · TCP ... ），
     避免按像素猜位置；
  3) 悬停该图标等 tooltip 弹出，连同 tooltip 一起 BitBlt 截屏存 PNG
     —— tooltip 文字直接证明该图标就是本程序；
  4) 再点一次 chevron 收起面板。

  兼容：Windows PowerShell 5.1 与 PowerShell 7 均可运行
       （C# 只做 Win32 调用，不引用 System.Drawing；截屏用 PowerShell 侧 System.Drawing）。
#>
param(
  # 默认输出 = <项目根>\docs\stage4-screenshots\10_pc_tray_icon.png（随脚本位置推导）
  [string]$Out = "",
  [string]$NameMatch = "PhotoBeam"
)

# 项目根 = 本脚本所在 tools\ 的上一级
$proj = Split-Path -Parent $PSScriptRoot
if (-not $Out) { $Out = Join-Path $proj "docs\stage4-screenshots\10_pc_tray_icon.png" }

$ErrorActionPreference = 'Continue'
Add-Type -AssemblyName System.Drawing
Add-Type -AssemblyName UIAutomationClient
Add-Type -AssemblyName UIAutomationTypes

if (-not ("TrayWin" -as [type])) {
Add-Type -TypeDefinition @"
using System;using System.Runtime.InteropServices;
public class TrayWin {
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint f, uint dx, uint dy, uint d, IntPtr e);
  [DllImport("user32.dll")] public static extern IntPtr GetDC(IntPtr h);
  [DllImport("user32.dll")] public static extern int ReleaseDC(IntPtr h, IntPtr dc);
  [DllImport("gdi32.dll")] public static extern bool BitBlt(IntPtr d, int x, int y, int w, int h, IntPtr s, int sx, int sy, int rop);
  public static void Click(int x, int y) {
    SetCursorPos(x, y);
    mouse_event(0x0002,0,0,0,IntPtr.Zero);
    mouse_event(0x0004,0,0,0,IntPtr.Zero);
  }
}
"@
}

function Invoke-Click([int]$x, [int]$y) {
  [TrayWin]::Click($x, $y)
  Start-Sleep -Milliseconds 350
}

function Save-Screen([int]$x, [int]$y, [int]$w, [int]$h, [string]$path) {
  $src = [TrayWin]::GetDC([IntPtr]::Zero)
  try {
    $bmp = New-Object System.Drawing.Bitmap $w, $h
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $dst = $g.GetHdc()
    [void][TrayWin]::BitBlt($dst, 0, 0, $w, $h, $src, $x, $y, 0x00CC0020)
    $g.ReleaseHdc($dst); $g.Dispose()
    $bmp.Save($path, [System.Drawing.Imaging.ImageFormat]::Png); $bmp.Dispose()
  } finally { [void][TrayWin]::ReleaseDC([IntPtr]::Zero, $src) }
}

$root = [System.Windows.Automation.AutomationElement]::RootElement
$cTask = New-Object System.Windows.Automation.PropertyCondition(
  [System.Windows.Automation.AutomationElement]::ClassNameProperty, "Shell_TrayWnd")
$tb = $root.FindFirst([System.Windows.Automation.TreeScope]::Children, $cTask)
if (-not $tb) { Write-Output "TASKBAR_NOT_FOUND"; exit 2 }
$btn = $null
foreach ($el in $tb.FindAll([System.Windows.Automation.TreeScope]::Descendants,
    [System.Windows.Automation.Condition]::TrueCondition)) {
  if ($el.Current.ClassName -eq 'Button' -and $el.Current.AutomationId -eq '1502') { $btn = $el; break }
}
if (-not $btn) { Write-Output "CHEVRON_NOT_FOUND"; exit 2 }
$br = $btn.Current.BoundingRectangle
$cx = [int]($br.X + $br.Width / 2); $cy = [int]($br.Y + $br.Height / 2)

$cOv = New-Object System.Windows.Automation.PropertyCondition(
  [System.Windows.Automation.AutomationElement]::ClassNameProperty, "NotifyIconOverflowWindow")

# 面板状态先归零（开着就关掉），再打开
$ov = $root.FindFirst([System.Windows.Automation.TreeScope]::Children, $cOv)
if ($ov) { Invoke-Click $cx $cy; Start-Sleep -Milliseconds 1200 }
Invoke-Click $cx $cy
$ov = $null
for ($i = 0; $i -lt 12; $i++) {
  Start-Sleep -Milliseconds 350
  $ov = $root.FindFirst([System.Windows.Automation.TreeScope]::Children, $cOv)
  if ($ov) { break }
}
if (-not $ov) { Write-Output "OVERFLOW_NOT_FOUND"; exit 2 }
Write-Output "OVERFLOW_OPEN"

$target = $null
for ($try = 0; $try -lt 12 -and -not $target; $try++) {
  foreach ($el in $ov.FindAll([System.Windows.Automation.TreeScope]::Descendants,
      [System.Windows.Automation.Condition]::TrueCondition)) {
    if ($el.Current.Name -like "*$NameMatch*") { $target = $el; break }
  }
  if (-not $target) {
    Start-Sleep -Milliseconds 500
    $ov = $root.FindFirst([System.Windows.Automation.TreeScope]::Children, $cOv)
    if (-not $ov) { Invoke-Click $cx $cy; Start-Sleep -Milliseconds 1200; $ov = $root.FindFirst([System.Windows.Automation.TreeScope]::Children, $cOv) }
  }
}
if (-not $target) { Write-Output "TRAY_ITEM_NOT_FOUND ($NameMatch)"; exit 2 }
$tr = $target.Current.BoundingRectangle
Write-Output "TRAY_ITEM name='$($target.Current.Name)' rect=($([int]$tr.X),$([int]$tr.Y),$([int]$tr.Width)x$([int]$tr.Height))"

# 悬停 → 等 tooltip
# 先把鼠标挪开（否则光标箭头正好压在 PhotoBeam 图标上），再抓一张没有 tooltip 的
[void][TrayWin]::SetCursorPos(60, 300)
Start-Sleep -Milliseconds 400
$panelShot = [System.IO.Path]::Combine([System.IO.Path]::GetDirectoryName($Out),
  ([System.IO.Path]::GetFileNameWithoutExtension($Out) + "_panel.png"))
Save-Screen ([int]($tr.X - 300)) ([int]($tr.Y - 110)) 620 210 $panelShot
Write-Output "SAVED_PANEL $panelShot"

[void][TrayWin]::SetCursorPos([int]($tr.X + $tr.Width / 2), [int]($tr.Y + $tr.Height / 2))
Start-Sleep -Milliseconds 2800

$x = [int]($tr.X - 300); if ($x -lt 0) { $x = 0 }
$y = [int]($tr.Y - 110); if ($y -lt 0) { $y = 0 }
Save-Screen $x $y 620 210 $Out
Write-Output "SAVED $Out"

Invoke-Click $cx $cy
exit 0
