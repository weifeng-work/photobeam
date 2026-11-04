#!/bin/bash
# PhotoBeam APK 构建（无 Gradle：aapt2 → kotlinc → d8 → zipalign → apksigner）
# 依赖：JDK 21（已装）、Android SDK（已装）、kotlinc（tools/kotlinc）
set -e
cd "$(dirname "$0")"

# SDK 根目录：优先认环境变量（CI runner 的 ANDROID_SDK_ROOT 与本机路径不同），
# 都没有才退回 Windows 默认位置 %LOCALAPPDATA%\Android\Sdk
SDK="${ANDROID_SDK_ROOT:-${ANDROID_HOME:-$LOCALAPPDATA/Android/Sdk}}"
BT="$SDK/build-tools/37.0.0"
PLATFORM="$SDK/platforms/android-36/android.jar"
KOTLINC="tools/kotlinc/bin/kotlinc.bat"
KSTDLIB="tools/kotlinc/lib/kotlin-stdlib.jar"
BCJAR="tools/bcprov.jar"
PKG="com.photobeam.app"
SRC="android/app/src/main/kotlin"
MANIFEST="android/app/src/main/AndroidManifest.xml"
OUT="android/build"

mkdir -p "$OUT/dex" "$OUT/gen"

rm -f PhotoBeam.apk "$OUT/base.apk" "$OUT/unsigned.apk" "$OUT/aligned.apk"
echo "[1/5] aapt2 compile res + link"
RESZIP="$OUT/res.zip"
if [ -d android/app/src/main/res ]; then
  "$BT/aapt2.exe" compile --dir android/app/src/main/res -o "$RESZIP"
  "$BT/aapt2.exe" link -o "$OUT/base.apk" -I "$PLATFORM" --manifest "$MANIFEST" \
    -R "$RESZIP" --java "$OUT/gen" --auto-add-overlay
else
  "$BT/aapt2.exe" link -o "$OUT/base.apk" -I "$PLATFORM" --manifest "$MANIFEST" \
    --java "$OUT/gen" --auto-add-overlay
fi

echo "[2/5] kotlinc"
find "$SRC" -name "*.kt" > "$OUT/kotlinc_args.txt"
find "$OUT/gen" -name "R.java" >> "$OUT/kotlinc_args.txt" 2>/dev/null || true
printf '%s
' "-classpath" "$PLATFORM;$BCJAR" >> "$OUT/kotlinc_args.txt"
"$KOTLINC" @"$OUT/kotlinc_args.txt" -d "$OUT/classes"

echo "[3/5] d8"
find "$OUT/classes" -name "*.class" > "$OUT/classlist.txt"
echo "$KSTDLIB" >> "$OUT/classlist.txt"
echo "$BCJAR" >> "$OUT/classlist.txt"
"$BT/d8.bat" --release --lib "$PLATFORM" --classpath "$KSTDLIB" \
  --output "$OUT/dex" $(cat "$OUT/classlist.txt")

echo "[4/5] 组装 dex 进 APK"
if [ ! -f "$OUT/dex/classes.dex" ]; then echo "classes.dex 缺失"; exit 1; fi
python - "$OUT" << 'PYIN'
import shutil, zipfile, sys
out = sys.argv[1]
shutil.copy(out + "/base.apk", out + "/unsigned.apk")
with zipfile.ZipFile(out + "/unsigned.apk", "a") as z:
    z.write(out + "/dex/classes.dex", "classes.dex")
print("dex added")
PYIN

if [ ! -f "$OUT/dex/classes.dex" ]; then echo "FATAL: classes.dex 缺失，构建失败"; exit 1; fi
echo "[5/6] zipalign + apksigner"
"$BT/zipalign.exe" -f 4 "$OUT/unsigned.apk" "$OUT/aligned.apk"

# 签名材料 [S3-5]：release keystore 存放在仓库之外（%LOCALAPPDATA%/PhotoBeam/），
# 口令从环境变量 PHOTOBEAM_KEYSTORE_PASS 读取（缺省时生成并提示保存）。
# 升级必须用同一把钥匙——请自行备份 keystore 与口令。
KEYSTORE_DIR="$LOCALAPPDATA/PhotoBeam"
KEYSTORE="$KEYSTORE_DIR/photobeam-release.keystore"
mkdir -p "$KEYSTORE_DIR"
if [ ! -f "$KEYSTORE" ]; then
  # 首次构建：生成 keystore + 随机口令并落盘保存 [S3-5]
  KS_PASS="${PHOTOBEAM_KEYSTORE_PASS:-pb-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
  keytool -genkeypair -keystore "$KEYSTORE" -storepass "$KS_PASS" -keypass "$KS_PASS" \
    -alias photobeam-release -keyalg RSA -keysize 2048 -validity 10000 \
    -dname "CN=PhotoBeam"
  echo "$KS_PASS" > "$KEYSTORE_DIR/keystore.pass"
  chmod 600 "$KEYSTORE_DIR/keystore.pass" 2>/dev/null || true
  echo "提示: 已生成 release keystore，口令保存在 $KEYSTORE_DIR/keystore.pass（请备份）"
else
  # keystore 已存在：口令优先取环境变量，否则读回已存口令（防止每次换口令解不开）
  if [ -n "$PHOTOBEAM_KEYSTORE_PASS" ]; then
    KS_PASS="$PHOTOBEAM_KEYSTORE_PASS"
  elif [ -f "$KEYSTORE_DIR/keystore.pass" ]; then
    KS_PASS="$(head -c 64 "$KEYSTORE_DIR/keystore.pass" | tr -d '\r\n')"
  else
    echo "FATAL: keystore 已存在但口令未提供（设 PHOTOBEAM_KEYSTORE_PASS 或恢复 keystore.pass）"; exit 1
  fi
fi
"$BT/apksigner.bat" sign --ks "$KEYSTORE" --ks-pass "pass:$KS_PASS" \
  --key-pass "pass:$KS_PASS" --out PhotoBeam.apk "$OUT/aligned.apk"

# 钥匙指纹防漂移 [S3-5 补强]：证书指纹一变 = 换了钥匙 = 新包无法覆盖升级已装设备
# （实测 2026-11-02：钥匙被重新生成过，真机上的包与新包指纹不同 → INSTALL_FAILED_UPDATE_INCOMPATIBLE）
FPR_FILE="$KEYSTORE_DIR/keystore.sha256"
CUR_FPR="$("$BT/apksigner.bat" verify --print-certs PhotoBeam.apk | grep 'SHA-256 digest' | head -1 | sed 's/.*digest: //' | tr -d ' \r')"
if [ -z "$CUR_FPR" ]; then
  echo "FATAL: 读不出签名证书指纹"; exit 1
fi
if [ -f "$FPR_FILE" ]; then
  OLD_FPR="$(tr -d ' \r' < "$FPR_FILE")"
  if [ "$CUR_FPR" != "$OLD_FPR" ]; then
    if [ "${PHOTOBEAM_ALLOW_KEY_ROTATION:-}" = "1" ]; then
      echo "警告: 证书指纹已变化且显式允许轮换"
      echo "警告: 旧=$OLD_FPR"
      echo "警告: 新=$CUR_FPR"
    else
      echo "FATAL: 签名证书指纹与记录不一致 —— 钥匙被换过！"
      echo "       记录=$OLD_FPR"
      echo "       当前=$CUR_FPR"
      echo "       新包将无法覆盖升级已装设备（签名不一致）。"
      echo "       若确实要换钥匙：确认已装设备全部重装后，设 PHOTOBEAM_ALLOW_KEY_ROTATION=1 重跑。"
      exit 1
    fi
  fi
fi
echo "$CUR_FPR" > "$FPR_FILE"
echo "签名证书指纹: $CUR_FPR（已记录到 $FPR_FILE）"

echo "[6/6] 三步交付校验 [S1-R-2/S3-4]"
# ① APK 内含 classes.dex + ② 特征串检索
python - << 'PYCHK'
import zipfile, sys
z = zipfile.ZipFile("PhotoBeam.apk")
names = z.namelist()
assert "classes.dex" in names, "APK 内缺 classes.dex"
data = z.read("classes.dex")
for s in (b"probeLinkAlive", b"completeImage", b"UpgradeReceiver"):
    assert s in data, "特征串缺失: " + s.decode()
print("PASS ① classes.dex 存在 ② 特征串完整")
PYCHK
# ③ apksigner verify（签名有效性 + 证书指纹）
"$BT/apksigner.bat" verify --print-certs PhotoBeam.apk | head -4
"$BT/apksigner.bat" verify PhotoBeam.apk && echo "PASS ③ apksigner verify 通过"

rm -f PhotoBeam.apk.idsig
echo "完成: $(pwd)/PhotoBeam.apk"
