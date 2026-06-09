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

## [v2.0.0] — 2026-06-09 (stable)

### Added

- Secure, integrated file manager across three surfaces, all on the existing authenticated, host-key-pinned SSH channel:
  - `fleet file list|upload|download|mkdir|rm|mv` and `fleet file defaults show|set` (global and per-server)
  - `fleet files <server>` — dual-pane terminal file manager with mouse drag-and-drop and live progress
  - `fleet ui` — localhost-only browser file manager with desktop drag-and-drop, an upload queue, and live progress
- Transfers are chunked, run over multiple concurrent `fleet-rpc` channels, are SHA-256-checksummed per chunk and whole-file, and resume after a drop or restart.
- Per-server and global file-transfer defaults (remote dir, parallel streams, chunk size), seeded on first connection.
- `fleet sync <server> <local-dir> <remote-dir>` — live one-way directory sync that pushes a folder once, then mirrors local changes (and, with `--delete`, removals) to the server until the command is stopped.
- Agentic control for AI coding agents:
  - `fleet context` — a complete, self-describing command reference generated live from the binary (`--json` for a structured tree)
  - `fleet ai <command>` — full machine-readable help (markdown or `--json`) for any single command; the AI-facing counterpart to `--help`. Both `context` and `ai` render from the live command tree, so they never need manual updating.
  - `fleet skill claude|codex|agents` — install a global skill / slash command so Claude Code or Codex can operate the fleet
- Robust terminal mouse support via bubblezone (content-anchored zones) in both the dashboard and the file manager.

### Changed

- The dashboard's mouse hit-testing was migrated from manual coordinate math to bubblezone.

### Security

- File transfers reuse the agent's path validation (absolute-only, symlink-resolved, `/proc`/`/sys`/`/dev` blocked) and add per-upload write/finalize locking, declared-size bounds (anti sparse-file abuse), transfer-id-keyed temp files, and overflow-safe range math.
- The web UI binds loopback only, requires a per-process token (constant-time compare), restricts mutations to POST with an Origin/CSRF check, caps upload body size, and sets a strict CSP and security headers.

### Fixed

- Guard against an `index out of range` panic in the dashboard log view when a cached log is marked available but has no lines.
