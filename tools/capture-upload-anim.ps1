<#
  PhotoBeam 传输动画验证（状态栏图标 4 帧轮转）

  原理：
    1) 生成 N 张内容互异的图（内容互异是硬要求：PC 侧按 SHA-256 file_id 幂等去重，
       内容相同的图会被判 duplicate，不会产生真正的传输）。
    2) 一次性 adb push 到 /sdcard/Pictures/pb_anim/（此时 MediaStore 行 is_pending=1，
       应用按 Stage3-R5 规则正确排除，不会误传）。
    3) 设备端状态栏连拍（screencap → /data/local/tmp，不落 /sdcard，避免污染被测队列）。
    4) 连拍开始后调 content call --method scan_volume 让全部图片一次性定稿
       → MediaStore 批量变更 → 应用 250ms 去抖合并成一次冲刷 → 一条连接发完整批
       → uploading 持续数秒 → 180ms/帧的 4 帧动画真正转起来。
    5) 拉帧后逐帧裁状态栏图标区取 SHA-256：多哈希 = 在动，单哈希 = 静止。

  用法：
    powershell -File tools\capture-upload-anim.ps1 -Count 40 -Tag anim4
#>
param(
  [int]$Count = 40,
  [string]$Tag = "anim",
  [string]$RemoteDir = "",
  # 默认输出目录 = <项目根>\docs\stage4-screenshots\frames（随脚本位置推导，不写死本机路径）
  [string]$OutDir = "",
  [int]$BurstFrames = 45,
  [double]$BurstInterval = 0.08,
  [int]$Seed = 20261102,
  # -NoBurst: 只做「生成→推送→批量定稿」，不自己连拍（配合外部 screenrecord 录屏抓帧）
  [switch]$NoBurst,
  # 状态栏 PhotoBeam 图标区（物理像素 1080x2400）：P 在 x=130..140，波点在 x≈143..147，
  # 右侧 x>=173 是别的应用图标，选区刻意排除，避免污染帧哈希。
  [int]$Rx = 108,
  [int]$Ry = 30,
  [int]$Rw = 62,
  [int]$Rh = 60
)

$ErrorActionPreference = 'Continue'
Add-Type -AssemblyName System.Drawing

# 项目根 = 本脚本所在 tools\ 的上一级
$proj = Split-Path -Parent $PSScriptRoot
if (-not $OutDir) { $OutDir = Join-Path $proj "docs\stage4-screenshots\frames" }
$imgDir = Join-Path $proj "docs\stage4-screenshots\genimgs_$Tag"
$localDir = Join-Path $OutDir $Tag
$iconDir = Join-Path $localDir "icons"
$burstDir = "/data/local/tmp/pb_burst"
# 每批用独立设备目录 + 独立文件名：同名文件会复用 MediaStore 的旧 _id，
# 而应用按 "_id > 水位" 过滤 → 复用即漏收（实测：对账新增 0）。
if ($RemoteDir -eq "") { $RemoteDir = "/sdcard/Pictures/pb_anim_$Tag" }
New-Item -ItemType Directory -Force -Path $localDir, $iconDir, $imgDir | Out-Null

Write-Output "== 1/5 生成 $Count 张内容互异测试图 =="
& python (Join-Path $proj "tools\gen-test-images.py") $imgDir $Count 1080 2340 92 $Seed
$files = Get-ChildItem $imgDir -Filter "*.jpg" | Sort-Object Name
Write-Output "   本地图片数: $($files.Count)"

Write-Output "== 2/5 推送到设备 $RemoteDir =="
adb shell "rm -rf $RemoteDir; mkdir -p $RemoteDir" 2>$null | Out-Null
$names = @()
foreach ($f in $files) { $names += $f.FullName }
& adb push @names "$RemoteDir/" 2>&1 | Select-Object -Last 2 | Out-String | Write-Output
adb shell "ls $RemoteDir | wc -l" 2>$null | Out-String | Write-Output

Write-Output "== 3/5 状态栏连拍（$BurstFrames 帧）=="
if ($NoBurst) {
  Write-Output "   -NoBurst：跳过连拍（外部录屏负责）"
} else {
adb shell "rm -rf $burstDir; mkdir -p $burstDir" 2>$null | Out-Null
# 连拍脚本先落到设备再执行：Start-Process 只传简单参数，避开 PowerShell 引号拆分问题
$burstSh = Join-Path $localDir "burst.sh"
$loopBody = "for i in `$(seq 1 $BurstFrames); do screencap -p $burstDir/f`$i.png; sleep $BurstInterval; done"
# 用 LF 且不带尾随换行写入：设备上的 sh 不认 CRLF
[System.IO.File]::WriteAllText($burstSh, $loopBody, (New-Object System.Text.UTF8Encoding($false)))
adb push $burstSh "$burstDir/burst.sh" 2>&1 | Select-Object -Last 1 | Out-String | Write-Output
$proc = Start-Process -FilePath adb -ArgumentList @("shell", "sh", "$burstDir/burst.sh") -PassThru -WindowStyle Hidden
Start-Sleep -Milliseconds 1500
}

Write-Output "== 4/5 触发批量定稿（scan_volume）→ 应用去抖合并成一次冲刷 =="
$t0 = Get-Date
adb shell content call --uri content://media/external/images/media --method scan_volume --arg external_primary 2>&1 | Out-String | Write-Output
Write-Output "   触发时刻: $($t0.ToString('HH:mm:ss.fff'))"

if (-not $NoBurst) {
  $proc.WaitForExit(300000) | Out-Null
}
Start-Sleep -Seconds 1

Write-Output "== 5/5 拉帧并逐帧哈希 =="
if ($NoBurst) {
  Write-Output "   -NoBurst：跳过拉帧（外部录屏负责抽帧）"
  Write-Output "DONE(no-burst) tag=$Tag 触发时刻=$($t0.ToString('HH:mm:ss.fff'))"
  exit 0
}
adb pull "$burstDir/." $localDir 2>&1 | Select-Object -Last 1 | Out-String | Write-Output
adb shell "rm -rf $burstDir" 2>$null | Out-Null

$frames = Get-ChildItem $localDir -Filter "f*.png" | Sort-Object { [int]($_.BaseName -replace '\D', '') }
if ($frames.Count -eq 0) { Write-Output "NO_FRAMES"; exit 2 }

$rows = @()
$hashes = @{}
$sha = [System.Security.Cryptography.SHA256]::Create()
foreach ($f in $frames) {
  $bmp = [System.Drawing.Bitmap]::FromFile($f.FullName)
  $rect = New-Object System.Drawing.Rectangle $Rx, $Ry, $Rw, $Rh
  $crop = $bmp.Clone($rect, $bmp.PixelFormat)
  $bmp.Dispose()
  $ms = New-Object System.IO.MemoryStream
  $crop.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
  $bytes = $ms.ToArray(); $ms.Dispose()
  $h = ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').Substring(0, 12)
  if ($hashes.ContainsKey($h)) { $n = $hashes[$h] } else { $n = $hashes.Count + 1; $hashes[$h] = $n }
  $rows += [pscustomobject]@{ Frame = $f.Name; Hash = $h; Unique = $n; Time = $f.LastWriteTime.ToString('HH:mm:ss.fff') }
  if (-not (Test-Path (Join-Path $iconDir ("u{0}_from_{1}.png" -f $n, $f.BaseName)))) {
    $big = New-Object System.Drawing.Bitmap ($Rw * 8), ($Rh * 8)
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
$verdict = if ($uniq -le 1) { '静止(STATIC)' } else { "在动(ANIMATING) $uniq 种帧" }
Write-Output "RESULT tag=$Tag frames=$($rows.Count) uniqueIconHashes=$uniq 判定=$verdict"
Write-Output "证据目录: $localDir"
Write-Output "逐帧放大图: $iconDir"
exit 0
