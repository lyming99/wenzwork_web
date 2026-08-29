@echo off
setlocal EnableExtensions
chcp 65001 >nul

cd /d "%~dp0"
title Wenzwork Device Agent

echo ==================================================
echo   Wenzwork Device Agent 正在启动...
echo ==================================================
echo.

if not exist "%~dp0Start.ps1" (
    echo [失败] 未找到启动脚本：%~dp0Start.ps1
    echo.
    pause
    exit /b 1
)

powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0Start.ps1" -Background
set "exitCode=%errorlevel%"

if not "%exitCode%"=="0" goto :start_failed

echo.
echo [成功] Wenzwork Device Agent 已启动并在后台运行。
echo [提示] 当前启动窗口将在 3 秒后关闭，程序将继续在后台运行。
timeout /t 3 /nobreak >nul
exit /b 0

:start_failed
echo.
echo [失败] Wenzwork Device Agent 启动失败，退出码：%exitCode%
echo [提示] 请检查以下错误日志：
echo        %~dp0runtime\logs\wenzwork-error.log
echo.
echo 按任意键关闭窗口...
pause >nul
exit /b %exitCode%
