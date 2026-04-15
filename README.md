# Cenvero Fleet

Command your fleet.

Cenvero Fleet is a self-hosted, operator-owned fleet management platform for Linux, macOS, and Windows servers. The controller runs on infrastructure you choose, stores its state in a directory you control, and manages remote nodes over encrypted SSH-based channels using both direct and reverse transport modes.

The full product name is **Cenvero Fleet** in prose and documentation. The controller binary is `fleet`, and the remote agent binary is `fleet-agent`.

## What Cenvero Fleet Does

Cenvero Fleet is built around a simple promise: one operator-controlled controller, one operator-controlled data directory, and no cloud dependency in the core runtime.

Today the repository includes:

- A controller binary with a Cobra CLI and a Bubble Tea terminal dashboard
- A remote agent binary with direct and reverse SSH transport support
- Direct-mode and reverse-mode session handling with TOFU host-key pinning
- Live service, logs, metrics, firewall, and port RPCs
- Metrics polling, alerting, suppression, acknowledgement, and desktop notifications
- Linux-first service management, firewall control, and remote bootstrap
- Controller-owned cached service logs with size, count, and age-based retention
- Managed database backends for SQLite, PostgreSQL, MySQL, and MariaDB
- Controller key rotation with rollout support for both direct and reverse fleets
- Controller and agent update flow with rollback support
- Static release assets under `public/` for GitHub Pages distribution
- CI, CodeQL, Goreleaser, manifest generation, signing sync, and release validation tooling

## Current Status

The codebase is feature-complete enough to behave like a serious late-stage pre-release build, not just a scaffold. The repository already includes the controller, agent, transport modes, TUI, release tooling, scale validation, and most of the operator workflows described in the project requirements.

Implemented now:

- `fleet init` creates the config layout, keys, databases, and audit paths
- `fleet dashboard` provides a multi-panel TUI with mouse and keyboard navigation
- `fleet server`, `service`, `logs`, `firewall`, `port`, `alerts`, `database`, `template`, `key`, `update`, and `config` command groups are present
- Reverse-mode reconnect resilience and queued metrics replay are implemented
- A 100-agent scale smoke test and release-readiness command are included locally

## Install

For the public one-command installer entrypoint:

```bash
curl -fsSL https://fleet.cenvero.org/install | sh
```

The `install` entrypoint dispatches to the correct hosted installer for the detected platform. On Linux and macOS it runs the POSIX installer directly. From a Windows-compatible shell such as Git Bash or WSL, it hands off to the PowerShell installer.

## Build From Source

The repository currently targets **Go 1.26**.

```bash
make test
make build
```

That produces the controller and agent binaries from:

- `./cmd/fleet`
- `./cmd/fleet-agent`

## Quick Start

### 1. Initialize the controller

```bash
fleet init
```

This creates the controller config directory, key material, database files, audit log, and default runtime configuration.

### 2. Add a server with automatic agent install

For Linux servers, pass `--login-user` to have the controller SSH in, download `fleet-agent` from the GitHub release, install it as a systemd service, and start it:

```bash
fleet server add web-01 192.0.2.10 \
  --mode direct \
  --login-user root \
  --login-key ~/.ssh/id_ed25519
```

Or with a sudo-capable user:

```bash
fleet server add web-01 192.0.2.10 \
  --mode direct \
  --login-user ubuntu \
  --sudo
```

The auto-install:
- Detects the server arch via `uname -m`
- Downloads the correct `fleet-agent` release binary
- Installs it to `/usr/local/bin/fleet-agent`
- Creates, enables, and starts `fleet-agent.service` via systemd

Removing a server tears it down automatically:

```bash
fleet server remove web-01
```

### 3. Direct mode: manual agent setup

Start an agent that accepts the controller public key:

```bash
./fleet-agent serve --authorized-keys ~/.cenvero-fleet/keys/id_ed25519.pub
```

Add it to the fleet:

```bash
./fleet server add demo 127.0.0.1 --mode direct --port 2222
./fleet server reconnect demo
./fleet service list demo
```

### 4. Reverse mode: agent dials out

Start the controller daemon:

```bash
./fleet daemon
```

Register the server on the controller:

```bash
./fleet server add edge-01 unknown --mode reverse
```

Then start the reverse agent on the remote server:

```bash
./fleet-agent reverse --controller controller.example.com:9443 --server-name edge-01
```

Once the reverse session comes up, the controller can use the same live service, metrics, logs, and alerting flows through that tunnel.

## Command Highlights

The current CLI surface is broad enough to manage the controller, servers, services, templates, keys, and updates from one binary.

Controller lifecycle:

- `fleet init`
- `fleet status`
- `fleet dashboard`
- `fleet daemon`

Server management:

- `fleet server add <name> <ip> [--login-user root --login-key ~/.ssh/id_ed25519]`
- `fleet server list`
- `fleet server show <name>`
- `fleet server reconnect <name>`
- `fleet server bootstrap <name>`
- `fleet server metrics <name>`
- `fleet server remove <name>`

Shell access and remote execution:

- `fleet ssh <server>` — interactive root shell via fleet key
- `fleet exec <server> <command>` — run one command on one server
- `fleet exec --all <command>` — run one command across all servers concurrently

Service and log operations:

- `fleet service list <server>`
- `fleet service add <server> <name> --log <path> --critical`
- `fleet service start|stop|restart <server> <name>`
- `fleet service logs <server> <name> --follow`
- `fleet service logs <server> <name> --cached`

Fleet visibility and control:

- `fleet firewall status|enable|disable <server>`
- `fleet firewall add <server> "<rule>"`
- `fleet port list|open|close <server> <port>`
- `fleet alerts`
- `fleet alerts ack <id>`
- `fleet alerts suppress <id> --for 6h`

Configuration, templates, and keys:

- `fleet database show`
- `fleet database shift --backend postgres --dsn '...'`
- `fleet config show|validate|backup|restore|export|import`
- `fleet template list`
- `fleet template apply <server> <template>`
- `fleet key fingerprint`
- `fleet key rotate`

Updates:

- `fleet update check`
- `fleet update apply`
- `fleet update rollback`
- `fleet update channel stable|beta`

## Configuration Layout

By default the controller stores its working state under `~/.cenvero-fleet`, with a layout like this:

```text
~/.cenvero-fleet/
├── config.toml
├── instance.id
├── keys/
│   ├── id_ed25519
│   ├── id_ed25519.pub
│   ├── known_hosts
│   └── rotations/
├── servers/
├── templates/
├── logs/
│   ├── _aggregated/
│   └── _audit.log
├── alerts/
├── data/
│   ├── state.db
│   ├── metrics.db
│   └── events.db
├── backups/
└── tmp/
```

The controller keeps ownership of its data locally:

- SQLite is split into separate workload files instead of one monolithic DB
- Aggregated service logs are cached under the controller’s `logs/_aggregated/`
- Log cache retention is enforced by size, retained backup count, and maximum age
- `fleet config backup` and `fleet config restore` operate on the controller directory rather than a cloud service

## Database Backends

The default backend is SQLite, split into:

- `data/state.db`
- `data/metrics.db`
- `data/events.db`

The store layer also supports:

- PostgreSQL
- MySQL
- MariaDB

The repo uses GORM-managed queries for normal controller storage flows, which keeps user-controlled values on bound parameters instead of string-built SQL.

If you need to move the controller later:

```bash
fleet database shift --backend postgres --dsn 'postgres://user:pass@host:5432/fleet?sslmode=require'
```

The shift command copies state first and only updates `config.toml` after the migration succeeds.

## Updates and Releases

The current default update posture is intentionally conservative:

- channel: `stable`
- policy: `notify-only`

That means the controller does not silently auto-update by default. Operators explicitly choose when to run:

```bash
fleet update check
fleet update apply
fleet update rollback
```

Release artifacts are intended to be minisign-signed, and both the installers and the updater verify checksums and signatures before swapping binaries.

For repo-side release hardening, use:

```bash
make scale
make release-ready
```

`make scale` runs the opt-in 100-agent reverse-mode smoke test and dashboard benchmark.

`make release-ready` runs:

- release helper syntax checks
- `go test ./...`
- controller and agent builds
- release tooling smoke tests
- scale validation

## Documentation

Full documentation is available at **[fleet.cenvero.org/docs/](https://fleet.cenvero.org/docs/)**.

Markdown source lives under [`docs/`](docs/index.md):

- [Getting Started](docs/getting-started.md)
- [Transport Modes](docs/transport-modes.md)
- [Configuration and Storage](docs/configuration-and-storage.md)
- [Operations Guide](docs/operations.md)
- [Releases and Updates](docs/releases-and-updates.md)

## Limitations and Scope Notes

A few boundaries are intentional in the current codebase:

- Linux is the primary operational target for service management, firewall control, and remote bootstrap
- macOS and Windows agents support transport, metrics, inventory, and update flows, but some ops commands return typed unsupported-capability errors
- There is no web UI in v1

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, sign-off requirements, testing expectations, and contribution workflow.

## Security

See [SECURITY.md](SECURITY.md) for vulnerability reporting guidance and current security posture.

## License

Cenvero Fleet is distributed under `AGPL-3.0-or-later`.

Copyright (C) 2026 Cenvero / Shubhdeep Singh.

See [LICENSE](LICENSE), [COPYING](COPYING), and [NOTICE](NOTICE) for the licensing text and project notices.

New source files should use the SPDX identifier:

```text
AGPL-3.0-or-later
```
