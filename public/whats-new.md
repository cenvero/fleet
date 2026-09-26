# What's new in Cenvero Fleet

> Markdown version of <https://fleet.cenvero.org/whats-new.html>. The complete reference in one file is at <https://fleet.cenvero.org/llms-full.txt>.

Release notes for Cenvero Fleet, the open-source, self-hosted server fleet manager, newest first. The latest stable release is v2.4.3, released on 11 September 2026. Every release is signed; upgrade with `fleet update apply` or `brew upgrade cenvero-fleet`. The complete, line-by-line history lives in the changelog.

[Latest release](https://github.com/cenvero/fleet/releases) · [Full changelog](https://github.com/cenvero/fleet/blob/main/CHANGELOG.md)

## Next release

*In development*

Already on `main` and landing in the release after v2.4.3: a large performance pass, rebuilt terminal and web interfaces, and a round of safety and correctness fixes found by an end-to-end review of every command.

### Highlights

- **Much faster at scale:** fleet-wide `exec`, metrics polling, `health`, `top` and agent updates run servers concurrently; CLI calls reuse a running daemon's warm connection (about 550 → 90 ms per command at 50 ms latency); transfers and `fleet sync` are several times faster over latency.
- **Terminal dashboard** rebuilt as a live operations console with sparklines, filters and actions; the **terminal file manager** gains a transfer queue, previews, bookmarks and go-to.
- **Redesigned web UI** with light and dark themes, a phone layout, a command palette, previews, streaming downloads and a read-only **Fleet overview**.
- **`fleet start` / `fleet stop`** run and stop the daemon in the background, and `fleet status` shows whether it is running.
- **`fleet approve`** now shows the staged request, asks for confirmation and runs it with its original options.
- `--secret` values reach the whole remote command through its environment; the audit log records commands, approvals and token and secret changes.
- The daemon's local control socket is **mutually authenticated**, and paths containing `..` are refused everywhere before any file operation.

## v2.4.3

*Latest stable · Released 2026-09-11*

Restores SSH-first Linux agent onboarding while keeping fail-closed release verification, and fixes onboarding, port, version-display and updater regressions from the v2.4 line.

### Highlights

- Auto-install opens the host-key-pinned SSH connection first, detects the target with `uname`, and has the server fetch only its own agent archive and signature — verified on the controller before anything is installed.
- Onboarding refuses duplicate names early, keeps configured login and agent ports, and records clear `installing` / `failed` / `managed` states.
- Retrying, cancellation-aware updater and manifest downloads that never leak signed URL parameters.
- Agent versions and server states read the same across list, inventory, dashboard and notifications.

## v2.3.0

*Released 2026-06-21*

Shell completion overhaul, configurable job-log retention and session reconnect grace, and a safe re-pin path when a server's host key changes.

### Highlights

- **Host-key re-pin prompt** when a reinstalled server presents a new key (defaults to No; `--accept-new-host-key` for automation).
- **Server-name tab completion** everywhere, installed as a cached completion file with `fleet autocomplete install`.
- **`fleet config set`** for `job-log-retention` (default 7 days) and `session-grace` (default 10 minutes).

## v2.2

*Released 2026-06-12*

A new automation and access-control surface alongside a full-codebase security audit and four re-audit rounds.

### Highlights

- **Automation:** `exec --json/--timeout/--retry`, tags and inventory, `journal`, `top`, `cp`, notifications, cron, drift detection, playbooks, jobs, `doctor`, `health`, SSH tunnels, approvals, idempotency keys and a dead-man's switch.
- **RBAC scoped tokens** enforced controller-side and fail-closed, and **named secrets** encrypted at rest (AES-256-GCM) with output redaction.
- **Signed, anti-rollback updates** bound to the version in the signature, a hash-chained audit log, and pinned SSH algorithms.
- v2.2.1: on Homebrew installs, agent updates run through `fleet sync-agent` and Homebrew owns the controller binary.

## v2.1.0

*Released 2026-06-09*

A major file-manager release across the CLI, the terminal UI and the web UI.

### Highlights

- A built-in **editor with syntax highlighting** in both file managers.
- **Compress and extract** zip, tar.gz, tar.bz2, tar.xz and tar with `fleet file compress` / `extract`.
- Permissions, checksums, duplicate, new file, filters, sortable columns — and up to six panes in the web UI.
- `fleet file move` between servers and parallel directory transfers.

## v2.0

*Major release · Released 2026-06-09*

Fleet becomes a file-moving, AI-driven control plane — all over the same authenticated, host-key-pinned SSH channel.

### Highlights

- **Secure file manager** on three surfaces: the `fleet file` CLI, the dual-pane `fleet files` terminal UI and the localhost `fleet file ui` — chunked, parallel, SHA-256-checksummed and resumable.
- **Live directory sync** with `fleet sync`, choosing the writer side with `--from`.
- **Agentic control:** `fleet context`, `fleet ai` and `fleet skill claude|codex|agents`.
- Server-to-server copies, mouse support in the terminal UIs, and an agent `--file-root` sandbox.

## Upgrade in seconds.

The controller verifies every release before it replaces itself, and rolls the same version out to your agents.

```sh
fleet update apply            # self-managed installs
brew upgrade cenvero-fleet    # Homebrew, then: fleet sync-agent
```

[Update docs](https://fleet.cenvero.org/docs/#updates) · [Explore Agentic Fleet](https://fleet.cenvero.org/agentic.html)
