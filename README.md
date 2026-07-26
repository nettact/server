# server-lite

## 一键部署

```bash
curl -fsSL https://d.nettact.org/install.sh | bash
```

脚本源码位于 [`deploy/install.sh`](./deploy/install.sh)，完整说明见 [部署文档](../docs/deploy.md)。

运行:

```
go run ./cmd/nettact-lite --db ./nettact.db --addr :12450 --dev
```

- 单用户、无租户;首启自动创建管理员并打印一次性密码(登录后 Settings 改密,
  或 `nettact-lite passwd -db <路径>` 离线重置)。
- **全部 flag 参考、监听地址优先级、数据保留、TLS 与会话 Cookie**:见
  **[docs/server-config.md](../docs/server-config.md)**;单一事实来源为
  `nettact-lite --help`。
- Web UI **运行时自动下载**:编译时用 ldflags 烧入精确的
  [web-console](https://github.com/nettact/web-console) 版本(`ci/deps.env` 的
  `WEB_CONSOLE_VERSION`),首次启动检测到前端缺失时从其公开 GitHub Release
  下载(SHA256 校验)到 `-webui-dir`(默认 `<db 目录>/webui`)。下载完成前
  非 `/api` 路径返回内置占位页(503),API 与探针不受影响。
  - 开发构建(未烧版本,`Version=dev`)不下载;设 `NETTACT_WEBUI_LOCAL`
    指向本地构建好的 dist 即可直接服务。
  - 镜像/内网部署可用 `NETTACT_WEBUI_BASE_URL` 覆盖下载源,或预置
    `<webui-dir>/<version>/` 目录。
- `--dev` 开放 CORS 便于本地对接 Vite(web-console)。

依赖 [github.com/nettact/protocol](https://github.com/nettact/protocol) 与 [github.com/nettact/server-core](https://github.com/nettact/server-core)。本地多仓开发使用 `go.work`。
