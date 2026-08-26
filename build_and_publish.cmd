@echo off
setlocal
where pwsh.exe >nul 2>nul
if errorlevel 1 (
  echo ERROR: PowerShell 7 ^(pwsh.exe^) is required. 1>&2
  exit /b 1
)
pwsh.exe -NoLogo -NoProfile -File "%~dp0scripts\Build-And-Publish-Release.ps1" %*
exit /b %errorlevel%
