<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://nettact.org/brand/nettact-logo-horizontal-reverse.svg">
    <source media="(prefers-color-scheme: light)" srcset="https://nettact.org/brand/nettact-logo-horizontal.svg">
    <img alt="NetTact" src="https://nettact.org/brand/nettact-logo-horizontal.svg" width="280">
  </picture>
</p>

# NetTact Server

English | [简体中文](./README-zh.md)

NetTact Server is an all-in-one network monitoring server for homes, small offices, and self-hosted deployments. It combines NetTact Server Core, the HTTP API, Agent connectivity, fault detection, notifications, and the web console in a lightweight service backed by local storage — a single SQLite database for configuration and state plus an embedded time-series store for metric history.

Agents run on monitored devices, execute probes from each device's location, and actively push telemetry. The server centrally distributes monitoring targets, stores history, detects faults, and presents the results. The Server does not scan the network itself and never needs to initiate a connection to an Agent.

## When to Use It

- Monitor network quality across a home, studio, or small office.
- Manage PCs, servers, NAS devices, routers, and remote machines from one place.
- Keep configuration, metrics, incidents, and notifications on infrastructure you control.
- Retain latency, packet-loss, DNS, HTTP, host, and Wi-Fi history.
- Investigate outages with incidents, path diagnostics, fluctuations, and availability history.

The server uses a single-admin, single-site model and supports up to 50 Agents. For an all-in-one experience on one computer, use [NetTact Desktop](https://nettact.org/en/desktop).

## Key Advantages

- **Simple to operate**: one server application, one data directory (SQLite + embedded time-series store), and one web console, with no external database required.
- **Endpoint perspective**: Agents execute probes where users and devices actually connect.
- **Outbound-only Agents**: no Agent ports are exposed, which works well with NAT, firewalls, dynamic IPs, and remote networks.
- **Data ownership**: configuration, metrics, events, and alerts stay in your own storage.
- **Explainable incidents**: availability is complemented by incident evidence, fluctuations, traceroutes, and notification history.
- **Cross-platform collection**: Windows, Linux, macOS, and Docker Agents can report to one server.
- **Low operational overhead**: amd64/arm64 container images include health checks, database migrations, and graceful shutdown.

## Deployment and Usage

Operational guidance is maintained in the NetTact documentation:

- [Deployment](https://nettact.org/en/deploy): Docker Compose, self-hosting, first login, remote Agents, host monitoring, status, upgrades, backup and restore, uninstall, HTTPS, and troubleshooting.
- [Server configuration](https://nettact.org/en/server-config): every command-line option and environment variable, listeners, administrator credentials, retention, TLS, session cookies, and web-console resources.
- [Agent configuration](https://nettact.org/en/agent-config): Agent installation on each platform, enrollment tokens, permissions, probe-access scope, and operations.
- [Permission reference](https://nettact.org/en/permissions): Agent capabilities, permission presets, and platform differences.

Treat these pages and `nettact-server --help` as the source of truth for deployment commands, version options, and operational procedures. They are intentionally not duplicated in this README.

## Building from Source

The project requires Go 1.25 and sibling `protocol` and `server-core` modules in the same NetTact workspace. With the root `go.work` configured:

```bash
go test ./...
go build -o nettact-server ./cmd/nettact-server
```

The `server` package can also be embedded in another Go program. NetTact Desktop uses the same startup, database, and service-orchestration implementation.

## License

[GNU Affero General Public License v3.0](./LICENSE)
