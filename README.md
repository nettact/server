# server-lite

NetTact **自托管 Lite 服务端** —— 单二进制，把 [server-core](https://github.com/nettact/server-core) 模块装配在一个 SQLite 数据库上（架构 §7）。AGPL-3.0。

运行：

```
go run ./cmd/nettact-lite --db ./nettact.db --addr :8080 --dev
```

- 单用户、无租户；Web UI（M4 起）经 `go:embed` 打进二进制。
- `--dev` 开放 CORS 便于本地对接 Vite（[web-console](https://github.com/nettact/web-console)）。

依赖 [github.com/nettact/protocol](https://github.com/nettact/protocol) 与 [github.com/nettact/server-core](https://github.com/nettact/server-core)。本地多仓开发使用 `go.work`。
