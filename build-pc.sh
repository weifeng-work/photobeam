#!/bin/bash
# PhotoBeam PC 端构建（发布版）
# 产物：dist/PhotoBeam.exe（GUI 子系统，双击无黑框）
# 说明：
#   - pc/PhotoBeam.exe 仍为控制台开发版（--test-clipboard 调试方便），不入交付
#   - GUI 子系统下 stdout 不存在，--test-clipboard 的输出走 logf → %LOCALAPPDATA%\PhotoBeam\logs\pc.log
#   - 交付校验：产物必须存在且 >4MB、子系统字段必须为 2（GUI）
set -e
cd "$(dirname "$0")/pc"

echo "[1/3] go vet（-unsafeptr=false 豁免：globalAllocCopy/globalRead 的 GlobalLock→Pointer
      转换是 x/sys 惯用模式，GlobalLock 期间内存被 pin，实为零风险——审查裁定见 stage3-review.md §七①）"
go vet -unsafeptr=false ./...  # 已在 pc/ 目录

# exe 文件图标：.ico → .syso（源码目录内，go build 自动链接）
if [ icon.ico -nt rsrc_windows_amd64.syso ]; then
  "$(go env GOPATH)/bin/rsrc.exe" -ico icon.ico -o rsrc_windows_amd64.syso
fi

echo "[2/3] go build（GUI 子系统）"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H=windowsgui" -o ../dist/PhotoBeam.exe .

echo "[3/3] 交付校验"
rm -f ../dist/PhotoBeam.exe~  # Go 在替换运行中的 exe 时会留 ~ 备份 [S3-2]
if [ ! -s ../dist/PhotoBeam.exe ]; then echo "FAIL: 产物不存在"; exit 1; fi
sz=$(stat -c %s ../dist/PhotoBeam.exe)
if [ "$sz" -lt 4000000 ]; then echo "FAIL: 产物过小 ($sz)"; exit 1; fi
# PE 子系统字段：偏移 0x3C 找 e_lfanew，PE+0x5C 是 Subsystem（2=GUI，3=CUI）
subsys=$(python - <<'EOF'
f = open('../dist/PhotoBeam.exe','rb').read()
pe = int.from_bytes(f[0x3c:0x40],'little')
print(int.from_bytes(f[pe+0x5c:pe+0x5e],'little'))
EOF
)
if [ "$subsys" != "2" ]; then echo "FAIL: 子系统=$subsys（应为 2=GUI）"; exit 1; fi
echo "完成: $(cd .. && pwd)/dist/PhotoBeam.exe (${sz}B, subsystem=GUI)"
