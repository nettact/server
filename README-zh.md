# NetTact Server

[English](./README.md) | 简体中文

NetTact Server 是面向家庭、小型办公室和自托管用户的一体化网络监控服务器。它把 NetTact Server Core、HTTP API、Agent 连接服务、故障判断、通知和 Web 控制台组织在一个轻量服务中，数据全部落在本地——配置与状态存于单个 SQLite 数据库，指标历史存于内嵌的时序存储。

Agent 安装在需要监控的设备上，从设备所在的位置执行网络探测并主动上传数据；服务端负责集中下发监控目标、保存历史、判断故障并展示结果。服务端不会主动扫描网络，也不需要从公网连接到 Agent。

## 适合什么场景

- 在家庭、工作室或小型办公室集中监控网络质量
- 统一观察多台 PC、服务器、NAS、软路由或异地设备
- 希望数据保存在自己的服务器，而不是第三方云平台
- 需要长期保存延迟、丢包、DNS、HTTP、主机和 Wi-Fi 指标
- 需要故障事件、路径诊断、通知策略和历史可用率

服务端使用单管理员、单站点模式，最多管理 50 个 Agent。需要免部署的单机体验时，可改用 [NetTact Desktop](https://nettact.org/zh/desktop)。

## 核心优势

- **部署简单**：单个服务端程序、一个数据目录（SQLite + 内嵌时序存储）和 Web 控制台，不依赖外部数据库。
- **真实终端视角**：探测由 Agent 执行，结果反映设备所在网络的真实体验。
- **纯出站 Agent**：Agent 不开放端口，适合 NAT、防火墙、动态 IP 和异地网络。
- **数据自主**：配置、指标、事件与告警都保存在自己的存储中。
- **故障可解释**：除了可用率，还能保存故障现场、波动记录、路径追踪和通知历史。
- **跨平台采集**：可管理 Windows、Linux、macOS 和 Docker Agent。
- **低维护成本**：Docker 镜像支持 amd64/arm64，内置健康检查、数据库迁移和优雅停机。

## 部署与使用

NetTact 的操作说明统一维护在用户文档：

- [一键部署](https://nettact.org/zh/deploy)：Docker Compose、自托管安装、首次登录、远程 Agent、宿主机监控、状态查看、升级、备份与恢复、卸载、HTTPS 和故障排查。
- [Server 配置](https://nettact.org/zh/server-config)：全部命令行参数与环境变量、监听地址、管理员凭据、数据保留、TLS、会话 Cookie 和 Web 控制台资源。
- [Agent 配置](https://nettact.org/zh/agent-config)：各平台 Agent 安装、注册令牌、权限、探测访问范围和运行维护。
- [权限参考](https://nettact.org/zh/permissions)：Agent 能力、权限预设和平台支持差异。

实际部署命令、版本参数和运维步骤请以这些文档以及 `nettact-server --help` 为准。README 不重复维护，以免升级流程变化后出现两套说明。

## 从源码构建

本项目使用 Go 1.25，并依赖同一 NetTact 工作区中的 `protocol` 和 `server-core`。根目录 `go.work` 配置完成后：

```bash
go test ./...
go build -o nettact-server ./cmd/nettact-server
```

`server` 包也可以嵌入其他 Go 程序。NetTact Desktop 使用的就是同一套启动、数据库和服务编排逻辑。

## 许可证

[GNU Affero General Public License v3.0](./LICENSE)
