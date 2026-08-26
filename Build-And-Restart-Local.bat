@echo off
setlocal EnableExtensions

cd /d "%~dp0"

set "POWERSHELL_EXE="
where pwsh.exe >nul 2>nul
if not errorlevel 1 set "POWERSHELL_EXE=pwsh.exe"
if not defined POWERSHELL_EXE set "POWERSHELL_EXE=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"

"%POWERSHELL_EXE%" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\Build-And-Restart-Local.ps1" %*
set "SCRIPT_EXIT_CODE=%ERRORLEVEL%"

echo.
if "%SCRIPT_EXIT_CODE%"=="0" (
  echo Local build and restart finished successfully.
) else (
  echo Local build and restart failed with exit code %SCRIPT_EXIT_CODE%.
)

exit /b %SCRIPT_EXIT_CODE%
