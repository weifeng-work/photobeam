<#
  PhotoBeam 文件夹选择器回归检查（Stage 5 收尾新增）
  用法：powershell -File tools\assert-folder-picker.ps1 -Out <输出png>
  原理：
    1) 找到配对向导窗（类名 PhotoBeamPairing），用 PostMessage(BM_CLICK) 触发「浏览…」
       （控件 ID 1004，与界面文案无关；用消息而不是鼠标坐标点击，不受遮挡/焦点影响）。
    2) 找出**带 SysTreeView32 子控件**的 #32770 —— 那才是文件夹选择器
       （向导窗自己的对话框、消息框都没有目录树）。
    3) 直接问目录树要节点数：TVM_GETCOUNT。
    4) 抓图存档。

  判定：TREE_ITEM_COUNT 必须 > 0。
  历史故障（Stage 5 用户实机反馈「选择图片接收文件夹预览窗口空白」）：
    根因是 SHBrowseForFolder 的 pidlRoot 槽位被塞了路径字符串（不是 PIDL），
    shell 解析出垃圾 → 目录树 0 个节点。本脚本就是该故障的守门人。

  前置条件：屏幕上有一个配对向导窗（先在手机上点「② 配对电脑」）。
  退出码：0=通过；2=找不到向导窗；3=找不到「浏览…」按钮；4=对话框没打开/没有目录树；
          5=目录树为空（回归了）。
#>
param(
  [string]$Out = "",
  [int]$TimeoutSec = 15
)

# 项目根 = 本脚本所在 tools\ 的上一级
$proj = Split-Path -Parent $PSScriptRoot
if (-not $Out) { $Out = Join-Path $proj "folder_picker_capture.png" }

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing

if (-not ("BpCheck" -as [type])) {
Add-Type @"
using System;
using System.Text;
using System.Runtime.InteropServices;
public class BpCheck {
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll")] public static extern bool EnumChildWindows(IntPtr parent, EnumProc cb, IntPtr l);
  [DllImport("user32.dll")] public static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern int GetWindowTextW(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern int GetDlgCtrlID(IntPtr h);
  [DllImport("user32.dll")] public static extern bool PostMessageW(IntPtr h, uint msg, IntPtr wp, IntPtr lp);
  [DllImport("user32.dll")] public static extern IntPtr SendMessageW(IntPtr h, uint msg, IntPtr wp, IntPtr lp);
  [DllImport("user32.dll")] public static extern IntPtr GetDC(IntPtr h);
  [DllImport("user32.dll")] public static extern int ReleaseDC(IntPtr h, IntPtr dc);
  [DllImport("gdi32.dll")] public static extern bool BitBlt(IntPtr dst, int x, int y, int w, int h, IntPtr src, int sx, int sy, uint rop);
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  public static IntPtr FindTop(string cls) {
    IntPtr found = IntPtr.Zero;
    EnumWindows((h, l) => {
      var sb = new StringBuilder(256); GetClassName(h, sb, 256);
      if (sb.ToString() == cls && IsWindowVisible(h)) { found = h; return false; }
      return true;
    }, IntPtr.Zero);
    return found;
  }
  public static IntPtr FindChildByClass(IntPtr parent, string cls) {
    IntPtr found = IntPtr.Zero;
    EnumChildWindows(parent, (h, l) => {
      var sb = new StringBuilder(256); GetClassName(h, sb, 256);
      if (sb.ToString() == cls) { found = h; return false; }
      return true;
    }, IntPtr.Zero);
    return found;
  }
  public static IntPtr FindChildById(IntPtr parent, int id) {
    IntPtr found = IntPtr.Zero;
    EnumChildWindows(parent, (h, l) => {
      if (GetDlgCtrlID(h) == id) { found = h; return false; }
      return true;
    }, IntPtr.Zero);
    return found;
  }
}
"@
}

# 1) 找配对向导窗
$wiz = [BpCheck]::FindTop("PhotoBeamPairing")
if ($wiz -eq [IntPtr]::Zero) {
  Write-Output "FAIL: 没有找到配对向导窗（类名 PhotoBeamPairing）——请先在手机上点「② 配对电脑」"
  exit 2
}
Write-Output "向导窗 hwnd=$wiz"

# 2) 触发「浏览…」（控件 ID 1004）
$btn = [BpCheck]::FindChildById($wiz, 1004)
if ($btn -eq [IntPtr]::Zero) {
  Write-Output "FAIL: 向导窗里没有「浏览…」按钮（控件 ID 1004）"
  exit 3
}
[void][BpCheck]::PostMessageW($btn, 0x00F5, [IntPtr]::Zero, [IntPtr]::Zero)  # BM_CLICK
Write-Output "已触发「浏览…」"

# 3) 等文件夹选择器（带目录树的 #32770）
$deadline = (Get-Date).AddSeconds($TimeoutSec)
$dlg = [IntPtr]::Zero
$tree = [IntPtr]::Zero
while ((Get-Date) -lt $deadline) {
  $dlg = [BpCheck]::FindTop("#32770")
  if ($dlg -ne [IntPtr]::Zero) {
    $tree = [BpCheck]::FindChildByClass($dlg, "SysTreeView32")
    if ($tree -ne [IntPtr]::Zero) { break }
  }
  Start-Sleep -Milliseconds 150
}
if ($tree -eq [IntPtr]::Zero) {
  Write-Output "FAIL: 文件夹选择器没有打开（找不到带目录树的 #32770）"
  exit 4
}
$dr = New-Object BpCheck+RECT
[void][BpCheck]::GetWindowRect($dlg, [ref]$dr)
$tb = New-Object System.Text.StringBuilder 256
[void][BpCheck]::GetWindowTextW($dlg, $tb, 256)
Write-Output "对话框 hwnd=$dlg title=$($tb.ToString())"

# 4) 目录树节点数（TVM_GETCOUNT = 0x1105）
Start-Sleep -Milliseconds 500
$cnt = [BpCheck]::SendMessageW($tree, 0x1105, [IntPtr]::Zero, [IntPtr]::Zero)
Write-Output "目录树节点数 = $cnt"

# 5) 抓图
$dw = $dr.Right - $dr.Left; $dh = $dr.Bottom - $dr.Top
$srcDC = [BpCheck]::GetDC([IntPtr]::Zero)
try {
  $bmp = New-Object System.Drawing.Bitmap $dw, $dh
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $dst = $g.GetHdc()
  [void][BpCheck]::BitBlt($dst, 0, 0, $dw, $dh, $srcDC, $dr.Left, $dr.Top, 0x00CC0020)
  $g.ReleaseHdc($dst); $g.Dispose()
  $bmp.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
  $bmp.Dispose()
  Write-Output "已保存 $Out"
} finally {
  [void][BpCheck]::ReleaseDC([IntPtr]::Zero, $srcDC)
}

if ([int]$cnt -le 0) {
  Write-Output "FAIL: 目录树是空的（节点数 0）——「选择图片接收文件夹预览窗口空白」回归了"
  exit 5
}
Write-Output "PASS: 目录树已正常渲染（节点数 $cnt）"
exit 0
