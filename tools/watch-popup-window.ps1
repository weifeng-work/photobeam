<#
  PhotoBeam 弹窗窗口监视器（诊断用，可复用）

  持续枚举 PhotoBeam.exe 进程的所有顶层窗口，只要窗口集合发生变化就打印，
  用于确认「弹窗窗口到底有没有被创建 / 类名是什么 / 是否可见」。

  用法：powershell -File tools\watch-popup-window.ps1 -Seconds 60 [-ProcessName PhotoBeam]
#>
param(
  [int]$Seconds = 60,
  [string]$ProcessName = "PhotoBeam"
)

$ErrorActionPreference = 'Continue'
Add-Type @"
using System;using System.Text;using System.Runtime.InteropServices;
public class WWatch {
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern int GetWindowTextW(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  public static string[] List(uint wantPid) {
    var sb = new System.Collections.Generic.List<string>();
    EnumWindows((h,l) => {
      uint pid; GetWindowThreadProcessId(h, out pid);
      if (pid == wantPid) {
        var cn = new StringBuilder(256); GetClassName(h, cn, 256);
        var t = new StringBuilder(256); GetWindowTextW(h, t, 256);
        RECT r; GetWindowRect(h, out r);
        sb.Add(h + "|vis=" + IsWindowVisible(h) + "|cls=[" + cn.ToString() + "]|title=[" + t.ToString() +
               "]|rect=" + r.Left + "," + r.Top + "," + (r.Right-r.Left) + "x" + (r.Bottom-r.Top));
      }
      return true;
    }, IntPtr.Zero);
    return sb.ToArray();
  }
}
"@

$proc = Get-Process -Name $ProcessName -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $proc) { Write-Output "NO_PROCESS: $ProcessName 未运行"; exit 2 }
Write-Output "WATCH pid=$($proc.Id) for ${Seconds}s"

$last = ""
$deadline = (Get-Date).AddSeconds($Seconds)
while ((Get-Date) -lt $deadline) {
  $cur = [string]::Join(" ;; ", [WWatch]::List([uint32]$proc.Id))
  if ($cur -ne $last) {
    Write-Output ("[{0}] {1}" -f (Get-Date).ToString('HH:mm:ss.fff'), $cur)
    $last = $cur
  }
  Start-Sleep -Milliseconds 100
}
Write-Output "WATCH_DONE"
