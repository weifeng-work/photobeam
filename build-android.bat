@echo off
rem PhotoBeam Android build entry (Windows) - ASCII only on purpose:
rem cmd.exe parses this file with the console OEM code page; UTF-8 Chinese bytes
rem get split into bogus commands ("... is not recognized"). Keep this file ASCII.
rem Usage: build-android.bat   (requires Git Bash on PATH)
rem Output: PhotoBeam.apk (zipaligned + apksigner signed, 3-step self-check inside)
cd /d "%~dp0"
bash build-android.sh
if errorlevel 1 (
  echo.
  echo [FAIL] build failed - see output above.
  exit /b 1
)
echo.
echo [OK] artifact: %~dp0PhotoBeam.apk
echo install: adb install -r PhotoBeam.apk
