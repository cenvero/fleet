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
