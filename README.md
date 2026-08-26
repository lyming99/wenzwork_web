# # Wenzwork

## 简介

WenzWork是一个跨平台的远程管理ai任务的工具，支持一个客户端调度多个设备，包括Windows、MacOS、Linux。

通过DeepSeek Harness类似的ai对话讨论需求；

通过AutoPlan类似的需求管理系统管理开发任务；

通过WenzMark类似的Markdown文件直接执行任务。

包含4个端：admin/host、relay、device、client。

## 项目模块

- admin/host: 管理服务端，管理员可以管理账户，普通会员可以管理远程设备。

- relay: 中继端，用于网路穿透p2p，加密通信，relay不可看奥具体内容。

- device: ai对话，任务中心，终端管理，文件管理真正实现的地方。

- client: flutter实现的客户端程序，安装即可打开。

## 产品资料

1. flutter 实现跨平台客户端client：ai对话 + 任务中心 + 终端管理 + 文件管理

2. go语言实现高性能device agent

3. go语言+vue实现管理端host和中继服务relay

4. 手机端演示视频：[https://www.bilibili.com/video/BV1398o6tEFh](https://www.bilibili.com/video/BV1398o6tEFh)

5. 远程通信实现原理视频：[https://www.bilibili.com/video/BV14mh36ZEBq/](https://www.bilibili.com/video/BV14mh36ZEBq/)

6. 测试站点(临时测试)：[https://work.wenzflow.com](https://work.wenzflow.com)

7. 前端项目：\[[https://github.com/lyming99/wenzwork](https://github.com/lyming99/wenzwork)\]([https://github.com/lyming99/wenzwork\_web](https://github.com/lyming99/wenzwork_web))

8. 后端项目：\[[https://github.com/lyming99/wenzwork\\\_web](https://github.com/lyming99/wenzwork\_web)\]([https://github.com/lyming99/wenzwork\_web](https://github.com/lyming99/wenzwork_web))

9. 手机端项目：保留项目，看情况，先保留版权偷偷上架~

## 开源许可

- MIT license