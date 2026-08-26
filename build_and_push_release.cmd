@echo off
setlocal
pwsh -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\Build-And-Push-Release.ps1" %*
exit /b %errorlevel%
