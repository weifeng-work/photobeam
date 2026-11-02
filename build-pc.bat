@echo off
rem PhotoBeam PC build entry (Windows) - ASCII only on purpose:
rem cmd.exe parses this file with the console OEM code page; UTF-8 Chinese bytes
rem get split into bogus commands ("... is not recognized"). Keep this file ASCII.
rem Usage: build-pc.bat   (requires Git Bash on PATH)
rem Output: dist\PhotoBeam.exe (GUI subsystem, PE subsystem=2 verified by the script)
cd /d "%~dp0"
bash build-pc.sh
if errorlevel 1 (
  echo.
  echo [FAIL] build failed - see output above.
  exit /b 1
)
echo.
echo [OK] artifact: %~dp0dist\PhotoBeam.exe
