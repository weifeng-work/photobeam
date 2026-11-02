<#
  PhotoBeam 通知图标帧对比工具（可复用回归）

  用途：判定 Android 状态栏的 PhotoBeam 图标是「静止」还是「在动」。
  判据 = 把状态栏图标区域的像素逐帧取 SHA-256：
    - 全部帧哈希相同  → 静止（空闲不闪）
    - 出现多个哈希    → 在动（动画在播）

  用法：
    powershell -File tools\capture-notif-frames.ps1 -OutDir <目录> -Count 20 -IntervalSec 0.15
    powershell -File tools\capture-notif-frames.ps1 -OutDir <目录> -Tag busy -Rx 265 -Ry 25 -Rw 50 -Rh 60

  说明：
  - 截图落在设备 /data/local/tmp/photobeam_frames（shell 可写，**不在 /sdcard**），
    避免被 MediaStore 索引成"新图片"进而触发 PhotoBeam 自己的同步队列，污染被测状态。
  - 每次运行结束会输出 frames.csv（帧号 + 区域哈希 + 唯一帧数）。
#>
param(
  # 默认输出目录 = <项目根>\docs\stage4-screenshots\frames（随脚本位置推导，不写死本机路径）
  [string]$OutDir = "",
  [int]$Count = 20,
  [double]$IntervalSec = 0.15,
  [string]$Tag = "idle",
  # 状态栏图标区域（物理像素，原图 1080x2400）
  [int]$Rx = 265,
  [int]$Ry = 25,
  [int]$Rw = 50,
  [int]$Rh = 60,
  # 连拍前先在设备端执行的命令（用于在抓帧窗口内触发"有负载"的状态变化）
  [string]$PreCmd = ""
)

# 项目根 = 本脚本所在 tools\ 的上一级
$proj = Split-Path -Parent $PSScriptRoot
if (-not $OutDir) { $OutDir = Join-Path $proj "docs\stage4-screenshots\frames" }

$ErrorActionPreference = 'Continue'
Add-Type -AssemblyName System.Drawing

$RemoteDir = "/data/local/tmp/photobeam_frames"
$localDir = Join-Path $OutDir $Tag
$iconDir = Join-Path $localDir "icons"
New-Item -ItemType Directory -Force -Path $localDir, $iconDir | Out-Null

Write-Output "== 设备端连拍 $Count 帧（间隔 ${IntervalSec}s）→ $RemoteDir =="
adb shell "rm -rf $RemoteDir; mkdir -p $RemoteDir" 2>$null | Out-Null
if ($PreCmd -ne "") {
  Write-Output "== 设备端预置命令：$PreCmd =="
  adb shell $PreCmd 2>&1 | Out-String | Write-Output
}
$cmd = 'for i in $(seq 1 ' + $Count + '); do screencap -p ' + $RemoteDir + '/f$i.png; sleep ' + $IntervalSec + '; done'
adb shell $cmd 2>$null | Out-Null

Write-Output "== 拉取到本地 =="
adb pull "$RemoteDir/." $localDir 2>$null | Out-Null
adb shell "rm -rf $RemoteDir" 2>$null | Out-Null

$frames = Get-ChildItem $localDir -Filter "f*.png" | Sort-Object { [int]($_.BaseName -replace '\D', '') }
if ($frames.Count -eq 0) { Write-Output "NO_FRAMES"; exit 2 }

$rows = @()
$hashes = @{}
$sha = [System.Security.Cryptography.SHA256]::Create()
foreach ($f in $frames) {
  $bmp = [System.Drawing.Bitmap]::FromFile($f.FullName)
  if ($Rx + $Rw -gt $bmp.Width -or $Ry + $Rh -gt $bmp.Height) {
    Write-Output "REGION_OUT_OF_BOUNDS: 图 $($bmp.Width)x$($bmp.Height)，区域 ${Rx},${Ry},${Rw},${Rh}"
  }
  $rect = New-Object System.Drawing.Rectangle $Rx, $Ry, $Rw, $Rh
  $crop = $bmp.Clone($rect, $bmp.PixelFormat)
  $bmp.Dispose()
  $ms = New-Object System.IO.MemoryStream
  $crop.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
  $bytes = $ms.ToArray(); $ms.Dispose()
  $h = ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').Substring(0, 12)
  if ($hashes.ContainsKey($h)) { $n = $hashes[$h] } else { $n = $hashes.Count + 1; $hashes[$h] = $n }
  $rows += [pscustomobject]@{ Frame = $f.Name; Hash = $h; Unique = $n }
  # 每个唯一帧另存一张图标区域放大图，作为人眼可查的证据
  if (-not (Test-Path (Join-Path $iconDir ("u{0}.png" -f $n)))) {
    $big = New-Object System.Drawing.Bitmap ($Rw * 6), ($Rh * 6)
    $g = [System.Drawing.Graphics]::FromImage($big)
    $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::NearestNeighbor
    $g.DrawImage($crop, 0, 0, $big.Width, $big.Height); $g.Dispose()
    $big.Save((Join-Path $iconDir ("u{0}_from_{1}.png" -f $n, $f.BaseName)), [System.Drawing.Imaging.ImageFormat]::Png)
    $big.Dispose()
  }
  $crop.Dispose()
}
$sha.Dispose()
$rows | Export-Csv (Join-Path $localDir "frames.csv") -NoTypeInformation -Encoding UTF8
$rows | Format-Table -AutoSize | Out-String -Width 120 | Write-Output

$uniq = ($rows.Hash | Select-Object -Unique).Count
Write-Output "RESULT tag=$Tag frames=$($rows.Count) uniqueIconHashes=$uniq 判定=$($(if ($uniq -le 1) { '静止(STATIC)' } else { '在动(ANIMATING)' }))"
Write-Output "证据目录: $localDir"
exit 0
