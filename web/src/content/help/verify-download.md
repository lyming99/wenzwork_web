---
title: 校验下载文件
description: 使用 SHA-256 确认安装包完整且与官网发布记录一致。
category: 安装与安全
order: 60
updatedAt: 2026-08-23
---

# 校验下载文件

WenzWork 下载页会为每个正式安装包公布 SHA-256。校验值一致，说明你拿到的文件与官网发布记录逐字节相同。

## Windows PowerShell

进入安装包所在目录并运行：

```powershell
Get-FileHash .\WenzWork-Setup.exe -Algorithm SHA256
```

## macOS

打开“终端”，运行：

```bash
shasum -a 256 WenzWork.dmg
```

## Linux

在终端运行：

```bash
sha256sum WenzWork.AppImage
```

把命令输出与[软件下载](/download)中对应平台的校验值完整比较。若有任意字符不同，请不要运行文件，删除它并从官网重新下载。

校验完整性不等于验证发布者身份。系统支持代码签名时，还应同时检查签名状态与发布者名称。
