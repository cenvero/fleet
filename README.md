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
- Persistent shell sessions that survive network drops with automatic reconnect (3 retries, 5 s gap)
- Live service, logs, metrics, firewall, and port RPCs
- Secure file manager with chunked, parallel, checksummed, resumable transfers over the same SSH channel — CLI (`fleet file`), dual-pane TUI (`fleet files`), and a localhost web GUI (`fleet file ui`)
- Structured remote execution (`fleet exec --json`) with timeouts, retries, dry-run, tag-group fan-out, and automatic output redaction
- Unattended-operation guardrails: scoped RBAC tokens (`fleet token` / `--token`), named secrets (`fleet secret`), a dead-man's-switch (`fleet guard`/`confirm`/`revert`), command policy (`fleet cmd-policy`), and an approval queue (`fleet approvals`/`approve`)
- Transactional, idempotent playbooks (`fleet run`) with check/apply/rollback, plus tag-based grouping (`fleet tag`) and scheduled jobs (`fleet cron`)
- Fleet observability: `fleet health`, `fleet top`, `fleet svc`, `fleet journal`, `fleet drift`, `fleet inventory --json`, and event notifications (`fleet notify`)
- Background jobs (`fleet job`/`jobs`), port tunneling (`fleet tunnel`), and health-gated rolling agent updates (`fleet agent update --canary`)
- Metrics polling, alerting, suppression, acknowledgement, and desktop notifications
- Linux-first service management, firewall control, and remote bootstrap
- Controller-owned cached service logs with size, count, and age-based retention
- Managed database backends for SQLite, PostgreSQL, MySQL, and MariaDB
- Controller key rotation with live verification and rollout for both direct and reverse fleets
- Controller and agent update flow with rollback support
- Config backup and point-in-time restore (`fleet backup`, `fleet config restore`)
- Post-reinstall config recovery (`fleet recover`)
- Versioned config migration wizard (`fleet adjust-init`)
- Static release assets under `public/` for GitHub Pages distribution
- CI, CodeQL, Goreleaser, manifest generation, signing sync, and release validation tooling

## Current Status

**v2** is a major upgrade: it adds a secure, integrated **file manager** across CLI, a dual-pane terminal UI, and a localhost browser UI, plus **agentic control** so AI coding agents can drive the fleet. The codebase already includes the controller, agent, transport modes, TUI, release tooling, scale validation, and most of the operator workflows described in the project requirements.

Implemented now:

- `fleet init` creates the config layout, keys, databases, and audit paths
- `fleet dashboard` provides a multi-panel TUI with mouse and keyboard navigation
- **Secure file manager (new in v2):** `fleet file` (CLI), `fleet files <server>` (dual-pane drag-and-drop TUI), and `fleet file ui` (localhost web file manager) — chunked, parallel, checksummed, resumable transfers
- **Live directory sync (new in v2):** `fleet sync` keeps a folder and a server directory mirrored — pick which side is the writer (`--from`); the replica is kept an exact copy (overwrite differing, delete extras) or `--no-delete` to keep extras — until you stop the command
- **Agentic control (new in v2):** `fleet context` and `fleet skill` let Claude Code / Codex learn and operate the whole fleet
- `fleet server`, `service`, `file`, `logs`, `firewall`, `port`, `alerts`, `database`, `template`, `key`, `update`, `backup`, `recover`, `adjust-init`, `config`, `ui`, `context`, and `skill` command groups are present
- Reverse-mode reconnect resilience and queued metrics replay are implemented
- Persistent shell sessions survive wifi drops and reconnect transparently
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
- Installs it to `/opt/cenvero-fleet/fleet-agent`
- Creates, enables, and starts `cenvero-fleet-agent.service` via systemd

Removing a server tears it down automatically and cleans up the stored host key:

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

The current CLI surface is broad enough to manage the controller, servers, services, templates, keys, updates, and backups from one binary.

Controller lifecycle:

- `fleet init`
- `fleet adjust-init`
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

- `fleet ssh <server>` — interactive shell via fleet key, survives network drops
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

Backup, restore, and recovery:

- `fleet backup` — create a timestamped `.tar.gz` of the config directory
- `fleet recover --from-dir <path>` — re-attach to a config directory after reinstall or migration
- `fleet config backup` — alias into config subcommand
- `fleet config restore <file>`

Configuration, templates, and keys:

- `fleet database show`
- `fleet database shift --backend postgres --dsn '...'`
- `fleet config show|validate|export|import`
- `fleet template list`
- `fleet template apply <server> <template>`
- `fleet key fingerprint`
- `fleet key rotate`
- `fleet key audit`
- `fleet key export-pub`

Updates:

- `fleet update check`
- `fleet update apply`
- `fleet update rollback`
- `fleet update channel stable|beta`

File manager and transfers (**new in v2**):

- `fleet file list <server> [path]` — browse a remote directory
- `fleet file upload <server> <local> [remote] [--parallel N] [--chunk-size 8M]`
- `fleet file download <server> <remote> [local] [--parallel N]`
- `fleet file mkdir|rm|mv <server> ...`
- `fleet file edit <server:path>` — edit a remote file in `$EDITOR`, then re-upload atomically
- `fleet file diff <serverA:path> <serverB:path>` (or `--group EXPR <path>`) — unified diff across servers
- `fleet file compress|extract|chmod|checksum|duplicate <server> ...` — archive, permission, and copy ops on the host
- `fleet file defaults show|set [server]` — per-server and global transfer defaults
- `fleet files [server...]` (also `fleet filemanager` / `fleet fm`) — desktop-grade **dual-pane** terminal file manager: each pane is Local or any server, with full operations (new folder, rename, delete, copy, move), a right-click menu, a hidden-file toggle, List/Icons views, and Finder-style drag-to-copy/move (local↔server **and** server↔server)
- `fleet file ui` (also `fleet filemanager ui`) — premium **dual-pane** localhost browser file manager (Local + server panes, same operations, drag-to-copy/move, desktop-drop upload, live progress)
- `fleet file copy <srcServer:path> <dstServer:path> [-r]` / `fleet file move …` — copy or move a file or directory **directly between two servers** (relayed through the controller)
- `fleet sync <server> <local-dir> <remote-dir> [--from local|remote] [--no-delete]` — live mirror: one side is the writer (source of truth, `--from`), the other a replica; the writer is copied once, then changes overwrite the replica and (by default) its extra files are deleted, until you stop the command

Transfers are chunked, run over multiple concurrent channels, are SHA-256-checksummed end to end, and resume after a drop — all on the same authenticated, host-key-pinned SSH channel.

Agentic control (**new in v2**):

- `fleet context` — print the full, self-describing command reference for an AI agent (add `--json`)
- `fleet ai <command>` — full machine-readable help for any one command (md or `--json`); the AI counterpart to `--help`
- `fleet skill claude|codex|agents` — install a global skill so your AI coding agent can drive Fleet

`context` and `ai` are generated live from the binary by walking the command tree, so they always match the installed version — there is nothing to keep in sync by hand, and any new command (with its help text) shows up automatically.

## Operating Safely and Unattended

Fleet is built to be driven programmatically and run unattended — by a script or an AI agent — with guardrails that keep a constrained credential from doing more than intended.

Structured remote execution:

- `fleet exec <server> <cmd> --json` — structured result (`stdout`/`stderr`/`exit_code`/`duration`)
- `fleet exec ... --timeout 30s --retry 2 --backoff 2s` — bounded, retried execution
- `fleet exec ... --group role=web` — fan out by tag; `--dry-run` previews; `--propagate-exit` surfaces the remote exit code
- `fleet exec ... --secret VAR=@name` — inject a stored secret as an env var (value redacted from all output)
- `fleet exec ... --guard` / `--confirm` / `--require-approval` / `--idempotency-key` — safety gates (below)

Access and safety:

- `fleet token create --name … [--servers … | --group EXPR] [--allow …] [--deny …] [--destructive]` — mint a scoped RBAC token; present it with `--token <id>` or `FLEET_TOKEN`. Enforcement is controller-side and fails closed.
- `fleet secret set|list|rotate|rm <name>` — named secrets, never echoed; referenced as `@name` from `exec --secret`
- `fleet guard <server> <cmd> --revert-after 2m --revert-cmd '<undo>'` — dead-man's-switch: auto-reverts unless you `fleet confirm <id>`; `fleet revert <id>` undoes now
- `fleet cmd-policy set deny|confirm <patterns>` — deny or confirm-gate dangerous commands
- `fleet approvals list` / `fleet approve <id>` — review and release commands staged with `exec --require-approval`
- `fleet policy set redact-pattern <regexes>` — reusable output-redaction patterns
- `fleet doctor <server>` — health checklist (agent, ports, disk, swap, reboot, clock)

Orchestration:

- `fleet run <playbook.yaml> [--group EXPR] [--on-fail rollback] [--dry-run]` — transactional, idempotent check/apply/rollback playbooks
- `fleet tag <server> key=value …` / `fleet tag --list` — label servers; `--group EXPR` (comma = AND) targets the matching set
- `fleet cron add|list|rm <server>` — manage scheduled jobs on a server

Observability:

- `fleet health [--json] [--group EXPR] [--watch]` — per-server checks (offline, swap, disk, reboot, clock skew, high load)
- `fleet top` — live CPU/mem/swap/disk/load table across servers
- `fleet svc <server> status|start|stop|restart|enable|disable <unit>` — structured systemd control
- `fleet journal <server> --unit <name> [--since 1h] [--follow]` — page or follow a unit's journal
- `fleet drift capture <server> --paths …` / `fleet drift <server>` — config-drift baseline and check
- `fleet inventory [--json] [--refresh]` — machine-readable fleet snapshot (OS, resources, ports, services, tags)
- `fleet notify add slack|webhook <url> --on …` — Slack/webhook targets that auto-fire on events (offline, job-failed, drift)

Background jobs, network, and agents:

- `fleet job run <server> <cmd>` / `fleet job status|wait|logs <id>` / `fleet jobs` — detached background jobs
- `fleet tunnel <server> <localPort>:<host>:<port>` — forward a local (loopback) port to a host reachable by the server
- `fleet agent version [--all]` — report agent versions and flag mismatches
- `fleet agent update [--all | --group EXPR] [--canary N]` — health-gated rolling agent update

Shell integration:

- `fleet automation set|get|list|rm <name>` — store named shell scripts
- `fleet shell-init [name] [--install]` — load the latest automation in every new shell
- `fleet autocomplete install` — enable tab-completion

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
│   ├── agents/
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
│   ├── events.db
│   └── control.token
├── backups/
└── tmp/
```

The controller keeps ownership of its data locally:

- SQLite is split into separate workload files instead of one monolithic DB
- Aggregated service logs are cached under the controller's `logs/_aggregated/`
- Log cache retention is enforced by size, retained backup count, and maximum age
- `fleet backup` and `fleet config restore` operate on the controller directory rather than a cloud service
- `data/control.token` is a per-session secret protecting the local reverse-hub control socket

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

Release artifacts are minisign-signed, and both the installers and the updater verify checksums and signatures before swapping binaries. All non-dev channels require at least a SHA-256 checksum in the manifest; a bare manifest entry is rejected.

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

## Agentic Fleet

Cenvero Fleet is built to be driven by AI coding agents (Claude Code, OpenAI Codex), not just humans. Because every command is a single `fleet` binary with JSON output and an SSH transport you control, an agent can operate your whole fleet safely from your terminal.

Install the integration once:

```bash
fleet skill claude      # ~/.claude/skills/cenvero-fleet/SKILL.md + a /fleet slash command
fleet skill codex       # ~/.codex/prompts/fleet.md
fleet skill agents      # a portable AGENTS.md
```

The installed skill is intentionally tiny — it tells the agent to run `fleet context` first, which prints the complete, always-current command reference, concepts, and safety guidance generated live from the installed binary (`fleet context --json` for a structured tree). After that, the agent can:

- inspect the fleet — `fleet status`, `fleet server list/show/metrics`, `fleet service list`, `fleet logs`
- **control any managed server** — start/stop services, manage the firewall and ports, run commands, rotate keys
- move files — `fleet file upload/download`, browse with `fleet file list`
- and guide you through it, asking before anything destructive

Everything the agent does rides the same authenticated, host-key-pinned SSH channel as the rest of the controller — there is no separate API surface or cloud dependency. You stay in control: the agent runs the `fleet` CLI on your machine, with your keys, against the servers you added.

## Documentation

Full documentation is available at **[fleet.cenvero.org/docs/](https://fleet.cenvero.org/docs/)**.

Markdown source lives under [`docs/`](docs/index.md):

- [Getting Started](docs/getting-started.md)
- [Transport Modes](docs/transport-modes.md)
- [Configuration and Storage](docs/configuration-and-storage.md)
- [File Manager and Transfers](docs/file-manager.md)
- [Agentic Fleet (AI control)](docs/agentic.md)
- [Operations Guide](docs/operations.md)
- [Releases and Updates](docs/releases-and-updates.md)

## Limitations and Scope Notes

A few boundaries are intentional in the current codebase:

- Linux is the primary operational target for service management, firewall control, and remote bootstrap
- macOS and Windows agents support transport, metrics, inventory, and update flows, but some ops commands return typed unsupported-capability errors
- The file-manager web UI (`fleet file ui`) was introduced in **v2** — it is localhost-only by design; there is still no remote/hosted web console, and the fleet dashboard remains terminal-based (`fleet dashboard`)

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
