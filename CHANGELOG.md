# Changelog

All notable changes to Cenvero Fleet are documented here.

The project follows [Keep a Changelog](https://keepachangelog.com/) and [Semantic Versioning](https://semver.org/).

## Retention Policy

This file is maintained automatically as part of the release process. It keeps:

- the 10 most recent **stable** releases
- the 10 most recent **beta** releases
- the 10 most recent **alpha** releases

Older entries are pruned when a new release of the same channel is appended. The full release history is available on the [GitHub Releases](https://github.com/cenvero/fleet/releases) page.

## Entry Format

Each release block follows this structure:

```
## [vX.Y.Z] — YYYY-MM-DD (stable | beta | alpha)

### Added
### Changed
### Fixed
### Security
### Removed
```

Omit sections that have no entries for that release.

<!-- releases appended below by the release workflow -->

## [Unreleased]

_Nothing yet — changes for the next release land here._

## [v2.2.1] — 2026-06-12 (stable)

Homebrew install ergonomics: agent updates are now driven solely by
`fleet sync-agent`, and the `update` / `self-uninstall` commands defer the
controller-binary lifecycle to Homebrew.

### Changed

- **Homebrew: `fleet update apply` no longer rolls out agents.** On Homebrew
  installs the controller binary is `brew`-managed, and agent updates are now
  driven exclusively by `fleet sync-agent`. `update apply` on Homebrew is
  informational only — it prints the `brew upgrade` command and points to
  `fleet sync-agent` (naming any agents whose stored version has drifted from
  the controller). Only `fleet update check` does real work on Homebrew;
  `apply`, `channel`, and `rollback` are blocked there.
- **Homebrew: `fleet self-uninstall` defers the binary to Homebrew.** It removes
  the config directory, then offers to run `brew uninstall cenvero-fleet`
  instead of trying to delete the brew-managed binary itself.
- **`fleet update check` on Homebrew surfaces drifted agents** and points to
  `fleet sync-agent` to bring them in line with the controller.

### Added

- **`App.AgentsNeedingSync`** — reports managed agents whose last-observed
  version differs from the controller (from versions already on disk; no network
  calls), powering the `sync-agent` nudges above.

### Fixed

- **`-race` test flake** — `TestCatRemoteFileAbortsWithoutEOF` no longer runs
  under `t.Parallel()` while mutating the package-global `maxCatRemoteBytes`
  that a concurrent test reads (test-only; no runtime change).

## [v2.2.0] — 2026-06-12 (stable)

A large security-hardening campaign (a full-codebase audit plus four follow-up
re-audit rounds) alongside a new automation/RBAC command surface, encrypted
secrets, signed anti-rollback updates, and operability improvements.

### Added

- **Automation command surface.** A broad batch of operator/automation commands:
  - `exec` enhancements — `--json`, `--timeout`, `--retry`, `--propagate-exit`,
    plus exec-time enforcement (dry-run, on-fail, group, guard, command policy,
    approval, idempotency, output redaction).
  - Stored automations with a shell-init loader and shell autocomplete.
  - Tags and inventory; service / `journal` / `top` views; `file edit`/`diff`,
    `cp`.
  - `notify`/webhooks, cron schedules, drift detection, policy/redaction, agent
    version reporting.
  - Playbooks, a dead-man's-switch, `doctor`, `jobs`, command policy, `health`,
    firewall (`fw`), approvals, and idempotency keys.
  - FL-028 SSH tunnels, FL-031 auto-firing notifications, FL-033 rolling
    updates, and FL-026 group diff.
- **Secrets (FL-004).** `secrets store` plus `exec --secret VAR=@name` to inject
  named secrets into the command environment, with values redacted from output.
- **RBAC scoped tokens (FL-030).** Controller-side enforcement of per-token
  scopes, with audited actions attributed to the active token.
- **Custom job names** (`job run --name`) and a **parallel `sync-agent`** with
  live per-server progress.
- **Periodic update check.** A background version check that surfaces a yellow
  "update available" notice regardless of how Fleet was installed.
- **`update apply --allow-unsigned` / `--allow-downgrade`** — explicit, opt-in
  overrides for an unsigned local build or a deliberate downgrade (fail-closed by
  default).
- **`fleet server enroll-token <name>`** to (re)mint a reverse-mode enrollment
  token.

### Changed

- Secrets are now encrypted at rest with AES-256-GCM, keyed from the
  controller key.
- Refreshed README, SECURITY, the public site, and the agent/AI context docs to
  describe fail-closed RBAC, per-token secrets, anti-rollback updates,
  `sync-agent` progress, and named jobs.

### Security

- **Full-codebase vulnerability audit remediation** across the agent sandbox,
  transport, file transfer, update flow, RBAC, secrets, and web UI.
- **Reverse-mode enrollment tokens** close the rogue-agent TOFU race in
  reverse-connect onboarding (HIGH).
- **Fail-closed RBAC** — re-audit round 2 made authorization deny-by-default and
  extended scope checks to `ssh`/`tag`/`doctor`/`template`; destructive `tag`
  operations and archive symlink handling were hardened (CRITICAL/HIGH). Token
  enforcement was extended to the `automation`, `autocomplete`, and `update`
  subcommands.
- **Re-audit round 2 mediums** — agent DoS bounds, transfer write-overflow,
  bootstrap MITM, firewall fail-open, `run` command-policy gaps, and per-token
  secret authorization.
- **Re-audit round 3** — update anti-rollback (refuse downgrade below the
  current/`min_supported` version), reverse-listener DoS cap with a post-auth
  timeout, file-diff memory bounds (8 MiB read cap + lower LCS bound), write
  `O_NOFOLLOW` and archive validate/extract TOCTOU fixes, probe allocation cap,
  job logfiles at `0600` with unpredictable names, plus round-3 low findings
  (permissions, DoS bounds, DNS-rebind, atomic key writes, SSRF, input
  validation).
- **Re-audit round 4** — verify-and-fix of the final audit pass: a lock around
  the reverse-enrollment verify-pin sequence (TOCTOU); SSH **KEX/MAC/host-key
  algorithm pinning** everywhere; `--accept-new-host-key` is first-connect-only;
  atomic `known_hosts`; an **audit-log SHA-256 hash chain** (tamper-evidence);
  update **version-binding** (assert the version in the signed minisign trusted
  comment); key-rotation **retention + secure-wipe**; cmd-policy gating of
  `ssh`/`svc`/`file edit`/crontab and **loopback-only tunnel targets** for scoped
  tokens; wider destructive-command classification; aggregate archive-extract
  caps and local `tar.xz` staging; and the remaining low findings (more
  `O_NOFOLLOW`, SQLite perms, IPv6 SSRF, atomic key writes, DoS bounds).
- Earlier audit waves: exec gate, key-rotation, job sentinel, and tunnel fixes;
  option-injection, firewall fail-open, deny-rule anchoring, and agent-error
  redaction; and a bounded file-diff LCS table (CodeQL
  `go/allocation-size-overflow`).

## [v2.1.0] — 2026-06-09 (stable)

A major file-manager release: an in-app editor, archives, more operations, and
faster directory transfers — across the CLI, the terminal UI, and the web UI.

### Added

- **File editor with syntax highlighting** in both the terminal (`e`) and web file
  managers — open a text file, edit, save (local and server); size-capped, binary-safe.
- **Compress / extract** in many formats (zip, tar.gz, tar.bz2, tar.xz, tar) — core
  engine plus `fleet file compress` / `fleet file extract` and context-menu actions in
  both UIs (runs the host's tar/zip on the target).
- **More file-manager operations** everywhere: permissions (chmod), SHA-256 checksum,
  duplicate, new file, filter/search, sortable columns, select-all, copy-path.
- **Web UI: up to 6 panes** — add/close panes and drag copy/move between any of them.
- **`fleet file move`** — move a file or directory directly between two servers.
- **`fleet filemanager` / `fleet filemanager ui`** — friendly aliases for the terminal
  and browser file managers.

### Changed

- **Parallel directory transfers** — recursive copy/move/upload/download now moves
  several files concurrently with aggregated progress (on top of per-file chunking).

### Security

- Local archive operations use a fixed tool with argv (no shell) and `./`-prefixed
  operands, eliminating command- and option-injection; remote paths stay shell-quoted.
- Fixed a data race in the parallel-transfer progress accounting; made web Duplicate
  collision-safe. Full adversarial review: no exploitable vulnerability.

## [v2.0.5] — 2026-06-09 (stable)

### Added

- Web file manager: a **"Local" source** (the controller's own filesystem) — panes now default to **Local ↔ first server**, with server↔server still selectable. Local↔server transfers use the upload/download engine; all local endpoints stay behind the loopback + token + CSRF guard.
- Web file manager: a Finder-style **List / Icons view toggle** (button + `v`), matching the terminal UI.
- Terminal file manager: richer, **type-distinct file icons** (dirs, code, docs, data, images, archives, media, executables, dotfiles), consistent across list, icon grid, and the drag ghost.

## [v2.0.4] — 2026-06-09 (stable)

### Added

- TUI file manager: a Finder-style **view toggle** (`v`) to switch a pane between List view and an Icons/Grid view; selection, open, and drag-and-drop work the same in both.

### Fixed

- Web file manager: the access token now persists in `sessionStorage`, so **refreshing the page no longer drops it** ("need token").
- Web file manager: much higher **text contrast** (file/dir names and metadata are clearly legible on the dark theme), and a stray empty modal box that appeared in the center is now hidden until opened.

## [v2.0.3] — 2026-06-09 (stable)

### Added

- **Desktop-grade dual-pane file managers.** Both `fleet files` (terminal, aliases `fleet filemanager` / `fleet fm`) and `fleet file ui` (browser) are now full file managers: each pane is the local filesystem or any server (browse local↔server **and** server↔server), with single-click select, double-click open, a right-click context menu and toolbar for every operation (new folder, rename, delete, copy, move, properties), a real-time hidden-files toggle, and Finder-style drag-and-drop — a cursor-following ghost, a glowing drop target, and a **Copy here · Move here · Cancel** menu on drop (same-pane drag = rename). Directory transfers confirm and copy the whole tree.
- **Server-to-server transfers.** `fleet file copy <srcServer:path> <dstServer:path> [-r]` copies a file or directory directly between two servers, relayed through the controller (reused by the UIs' drag Copy/Move). New `MoveFile`/`MoveDir` (rename within a server, copy-then-delete across).
- `fleet files` accepts **multiple servers** (`fleet files a b`) to open two at once; single-server (`fleet files a`) still works.

## [v2.0.1] — 2026-06-09 (stable)

### Changed

- The localhost web file manager now lives at `fleet file ui` (under the `file` command group). It previously had its own top-level command; that top-level name is reserved for a broader UI later.

### Fixed

- Release pipeline: GitHub Pages now deploys only after the manifest is committed and the release smoke test passes, and the `main` CI Pages deploy skips the release manifest commit — eliminating a race where the published site could deploy before/independently of the updated manifest.

## [v2.0.0] — 2026-06-09 (stable)

### Added

- Secure, integrated file manager across three surfaces, all on the existing authenticated, host-key-pinned SSH channel:
  - `fleet file list|upload|download|mkdir|rm|mv` and `fleet file defaults show|set` (global and per-server)
  - `fleet files <server>` — dual-pane terminal file manager with mouse drag-and-drop and live progress
  - `fleet file ui` — localhost-only browser file manager with desktop drag-and-drop, an upload queue, and live progress
- Transfers are chunked, run over multiple concurrent `fleet-rpc` channels, are SHA-256-checksummed per chunk and whole-file, and resume after a drop or restart.
- Per-server and global file-transfer defaults (remote dir, parallel streams, chunk size), seeded on first connection.
- `fleet sync <server> <local-dir> <remote-dir>` — live directory mirror. One side is the writer (source of truth, chosen with `--from local|remote`) and the other a read-only replica: the writer is copied once, then new/changed files overwrite the replica and, by default, replica files absent on the writer are deleted (`--no-delete` keeps them). Runs until stopped.
- Agentic control for AI coding agents:
  - `fleet context` — a complete, self-describing command reference generated live from the binary (`--json` for a structured tree)
  - `fleet ai <command>` — full machine-readable help (markdown or `--json`) for any single command; the AI-facing counterpart to `--help`. Both `context` and `ai` render from the live command tree, so they never need manual updating.
  - `fleet skill claude|codex|agents` — install a global skill / slash command so Claude Code or Codex can operate the fleet
- Robust terminal mouse support via bubblezone (content-anchored zones) in both the dashboard and the file manager.
- `fleet file stat|cat|tail` for inspecting remote files, and `-r/--recursive` on `fleet file upload|download` for whole directory trees.

### Changed

- The dashboard's mouse hit-testing was migrated from manual coordinate math to bubblezone.

### Security

- Controller no longer trusts agent-provided file names/paths when building local write paths (`sync --from remote` and TUI download): a compromised server cannot cause writes outside the target directory.
- `fleet-agent --file-root <dir>` confines all file operations to allowlisted directories (defense-in-depth against a stolen controller key).
- Update apply now requires a minisign **signature** on non-dev channels (a checksum alone is manifest-integrity-dependent).
- Agent log reads are memory-bounded (ring-buffered tail) so a huge log cannot OOM the agent; abandoned upload temp files are reaped.
- File transfers reuse the agent's path validation (absolute-only, symlink-resolved, `/proc`/`/sys`/`/dev` blocked) and add per-upload write/finalize locking, declared-size bounds (anti sparse-file abuse), transfer-id-keyed temp files, and overflow-safe range math.
- The web UI binds loopback only, requires a per-process token (constant-time compare), restricts mutations to POST with an Origin/CSRF check, caps upload body size, and sets a strict CSP and security headers.

### Fixed

- Guard against an `index out of range` panic in the dashboard log view when a cached log is marked available but has no lines.

## [v1.6.6] — 2026-06-06 (stable)

### Changed

- Updated `golang.org/x/crypto` to 0.52.0.

## [v1.6.5] — 2026-05-14 (stable)

### Changed

- Updated `golang.org/x/crypto` to 0.51.0.

## [v1.6.4] — 2026-05-06 (stable)

Promotion of `v1.6.4-beta.1` to stable.

### Changed

- Updated `golang.org/x/crypto` to 0.50.0.

## [v1.6.4-beta.1] — 2026-05-06 (beta)

### Fixed

- Fixed several `fleet update` edge cases.

## [v1.6.3] — 2026-04-26 (stable)

### Changed

- Routine dependency and CI-action bumps (`pgx/v5`, `actions/checkout`,
  `actions/download-artifact`, `goreleaser/goreleaser-action`).

<!--
Older stable releases (v1.6.2 and earlier) are pruned per the retention policy
(10 most recent stable). v1.6.3 is also the current min_supported stable.
Full history: https://github.com/cenvero/fleet/releases
-->

## [v1.3.3-beta.1] — 2026-04-15 (beta)

### Changed

- Use `HOMEBREW_TOKEN` for the `homebrew-fleet` formula push.

## [v0.1.0-alpha.1] — 2026-04-15 (alpha)

The initial public pre-release of Cenvero Fleet — a self-hosted,
operator-owned fleet manager for Linux, macOS, and Windows, with no cloud
dependency in the core runtime.

### Added

- **Controller (`fleet`) and agent (`fleet-agent`) binaries.** A Cobra CLI with
  a Bubble Tea terminal dashboard (multi-panel, mouse + keyboard), and an agent
  supporting both direct and reverse SSH transport.
- **Encrypted SSH-based control channel** with TOFU host-key pinning, for both
  direct-mode and reverse-mode sessions.
- **Core operator command groups:** `server`, `service`, `logs`, `firewall`,
  `port`, `alerts`, `database`, `template`, `key`, `update`, and `config`, plus
  `fleet init` to lay out config, keys, databases, and audit paths.
- **`fleet ssh` and `fleet exec`** for interactive shells and remote command
  execution over the agent's own channel.
- **Live RPCs** for services, logs, metrics, firewall, and ports; metrics
  polling with alerting, suppression, acknowledgement, and desktop
  notifications.
- **Automatic agent install and teardown** on `server add`/`remove`, with an
  install preview, interactive server add, password auth, and a `--no-agent`
  flag.
- **Linux-first service management, firewall control, and remote bootstrap;**
  controller-owned cached service logs with size/count/age retention.
- **Managed database backends** for SQLite, PostgreSQL, MySQL, and MariaDB.
- **Controller key rotation** with rollout support for direct and reverse
  fleets, and a controller/agent **update flow with rollback**.
- **Release/distribution tooling:** signed (minisign) release assets, a
  generated update manifest, a docs site under `public/`, and CI / CodeQL /
  Goreleaser pipelines.

### Security

- Fixed a race between `SaveServer`/`GetServer` on concurrent reverse connects.
- Resolved gofmt and gosec findings (G115/G302/G306) and a data race in the
  `memConn` test helper.
