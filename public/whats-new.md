# What's new in Cenvero Fleet

> Markdown version of <https://fleet.cenvero.org/whats-new.html>. The complete reference in one file is at <https://fleet.cenvero.org/llms-full.txt>.

Release notes for Cenvero Fleet, the open-source, self-hosted server fleet manager, newest first. The latest stable release is v2.7.0, released on 19 November 2026. Every release is signed; upgrade with `fleet update apply`, `brew upgrade cenvero-fleet` or `winget upgrade --id Cenvero.Fleet`. The complete, line-by-line history lives in the changelog.

[Latest release](https://github.com/cenvero/fleet/releases) · [Full changelog](https://github.com/cenvero/fleet/blob/main/CHANGELOG.md)

## v2.7.0

*Latest stable · Released 2026-11-19*

A web console that runs the whole fleet from the browser, a background helper that tracks jobs and keeps agents in sync without anyone asking, a firewall check that refuses to lock Fleet out of its agents, and commands that are never sent twice when they may already have run.

### Highlights

- **The web console:** `fleet console` (alias `fleet web`) opens the localhost web UI with every view — Fleet, Control, Files and Terminal. On a terminal it shows a panel instead of a bare URL: open it in your browser, show the access link, keep it running in the background, or stop it. A background console (also `fleet console --background`) is reached with `fleet console open [--files] [--print]`, `fleet console status` and `fleet console stop`, and stops by itself after an hour unused and after 8 hours at most (30 minutes and an hour with Control or Terminal on). The browser is opened with a one-time code, never the session token. `fleet file ui` opens the same console on the file manager, with Control and Terminal opt-in. [Docs](https://fleet.cenvero.org/docs/#web-console)
- **Run the fleet from the browser:** the Control view is a set of screens. *Servers* lists your servers straight away, each with its own page of details, usage and one-click actions; *Add server* is a step-by-step wizard; pages that only read load their data as tables as soon as they open, and actions have a button named for what they do. Each run is the `fleet` binary itself, so tokens, command policy, redaction, confirmations and the audit log apply exactly as in a terminal. The Terminal view opens `fleet ssh` shells on direct-mode servers in tabs, and with Control on, the Fleet overview gains *Add server* (the wizard), a per-server *Manage* menu and *Ack* / *Snooze 1h* on alerts.
- **`fleet errors`:** commands no longer print notices before their own output. `fleet errors` (also `fleet issues` and `fleet notices`; `--json`) lists what needs attention — an available update, a pending `fleet adjust-init`, a passphrase-protected controller key, servers the last automatic agent sync could not update, reverse-mode servers waiting for a daemon that is not running — each with the command that fixes it. Bare `fleet` lists them too, and `fleet status` prints a count. [Docs](https://fleet.cenvero.org/docs/#errors)
- **The background helper:** one small process per controller tracks running `fleet job`s, so they show as done (or lost) and `job-failed` is sent without anyone polling; it also runs the hourly agent sync and prunes old job logs. Any `fleet` command starts it, and there is nothing to set up. See it with `fleet helper status` and restart it with `fleet helper restart`. [Docs](https://fleet.cenvero.org/docs/#helper)
- **The firewall lockout check:** every command that can block traffic reads the server's ufw, iptables, nftables or firewalld rules, in the order the firewall applies them, and refuses a change that would cut Fleet off from the agent, naming the command to run first. Allowing the agent port only from the controller's IP, network or VPN interface is accepted, because Fleet checks the server's own view of its connection. `--force-i-have-console` is for a person at the server's console only, and is refused under any token. [Docs](https://fleet.cenvero.org/docs/#agent-lockout)
- **A command is never sent twice when it may already have run:** when a pooled connection dropped while waiting for a reply, the controller used to send the request again on a fresh one, so a command, a service restart or a firewall change could run twice. Now only requests that never reached the server, or that only read, are sent again; anything else fails with *may have run; check before retrying*. `fleet exec --retry` retries only failures that certainly never reached the server.

### Added

- `fleet console` and `fleet web`, with `open`, `status` and `stop`; `--control=false` and `--terminal=false` turn views off.
- The Control view: pages grouped by area, a Servers list and a page per server, the Add server wizard, data pages that load on their own (JSON and aligned text as tables), live output for actions, cancel, run history, pinned pages and quick actions. The command behind a page is under *Show the command*. An SSH password typed into the wizard reaches `fleet server add` on standard input (the new `--login-password-stdin`), never its command line. Commands that change things ask for confirmation in the browser, prompts fail closed unless you tick `--yes` or `--confirm`, and each run is audited as `webui.command`.
- The Terminal view: interactive shells on direct-mode servers, in tabs (xterm.js, vendored). `top` and `vim` work, the size follows the window and a reload re-attaches.
- `fleet errors` (`fleet issues`, `fleet notices`), `fleet helper status` and `fleet helper restart`.
- The `lost` job status: a job whose server was removed, or whose process is gone without a result (a reboot, a kill), ends as `lost` instead of `running` forever, and `fleet job wait` exits 1. [Docs](https://fleet.cenvero.org/docs/#jobs)

### Changed

- Automatic agent sync runs in the background helper instead of being launched by whichever command noticed it was due, and no longer prints a notice. `FLEET_AGENT_AUTOSYNC=off` now applies to the environment the helper starts from (then run `fleet helper restart`).
- The update notice, the `adjust-init` hint and the passphrase-key warning are no longer printed before other commands' output; `fleet errors` and bare `fleet` list them.
- `fleet fw enable --safe` no longer allows the agent port for you: it refuses and names `fleet fw allow <server> <port>/tcp`. Its JSON output now needs `--json`.
- `fleet firewall enable`, `fleet firewall add`, `fleet port close` and `fleet template apply` refuse changes that would lock Fleet out of the agent, and `template apply` opens the template's ports before enabling its firewall and changes services last.
- `fleet file ui` and `fleet filemanager ui` run the web console on the file manager and show its panel on a terminal; if a console already runs in the background, they open that one.
- A redesigned web UI: one blue accent, the system font, frosted toolbar, menus and dialogs, matching light and dark themes, and bundled Lucide icons. The tabs run Fleet → Control → Files → Terminal, the UI opens on Fleet, and `Alt`+`1`–`4` switch tabs. Fleet shows each server as a card with CPU, memory and disk ring gauges, or as a list.
- Dependencies: the archived `gopkg.in/yaml.v3` is replaced by `go.yaml.in/yaml/v3`, gopsutil moves to v4, and the `golang.org/x` modules are updated.

### Security

- The browser is opened with a one-time code that works once, within 30 seconds, instead of a URL carrying the session token, which every account on the machine could read from the process list. A link used a second time replaces the session token, and the page says so.
- A background console keeps its session token in memory only, is handed its listening socket when it moves to the background (Linux, macOS) so another account cannot take the port, and cannot be started or opened under `--token` / `FLEET_TOKEN`.
- In the Control view, commands that reach the controller's keys, tokens or secrets stay terminal-only, the controller's config directory is pinned ahead of every command, and arguments that would run a different command are refused.
- `--force-i-have-console` is refused under any token, with an audit entry, and every lockout refusal and override is audit-logged.
- Background jobs: a job's completion marker counts only once its wrapper process has exited, so a job, or whoever can influence its output, cannot forge *done, exit 0*.
- `fleet guard --revert-cmd` is held to the command policy, so a denied command can no longer be passed as the undo.
- A missing dedicated `secret.key` fails closed instead of being replaced by a new random key, and `fleet config import` refuses unsafe server names such as `../x`.
- Before signalling a pid from a pid or state file, `fleet stop` checks the process's command line or image on every OS, not only on Linux. Server text is cleaned of terminal escape sequences in firewall errors.
- Built with Go 1.26.11, which fixes GO-2026-6859 (an HTTP/1 server crash in `net/http`, reachable through the web UI's server), and `golang.org/x/crypto` 0.59.0, which clears GO-2026-5932. `govulncheck` reports no vulnerabilities in any required module.

### Fixed

- Background jobs: a job whose output does not end in a newline, or that leaves a child writing to its log, now finishes; `job-failed` is sent once, not twice; `job wait` and `job status` read a few hundred bytes instead of up to 4 MiB of log.
- A removed server no longer comes back: a connect, metrics collection, agent update or firewall, port or service call still running when it was removed no longer writes its record back.
- `fleet key rotate` no longer destroys stored secrets when it rolls back, and `fleet database shift` moves the metric history (the dashboard sparklines) too. MySQL and MariaDB state lookups no longer fail on the reserved word `key`.
- The agent always answers: a reply over the 16 MiB message limit is trimmed or answered with an error instead of being dropped, `shell.exec` returns at its deadline even when something it started keeps stdout open, and a handler panic no longer kills the agent.
- A pooled connection that sat through a laptop sleep is redialled instead of reused, and `fw enable --safe` says the revert timer is armed, and how to cancel it, when the enable step fails after arming it.
- `fleet stop` waits until the daemon has let go of its lock, so on Windows a `fleet start` right after no longer finds the old one apparently still running.
- File managers: in the web UI, new file, rename and same-pane move never overwrite an existing file; in `fleet files`, the editor keeps tabs and CRLF line endings exactly, a single Esc no longer discards unsaved edits, and local saves are atomic and keep owner, group and mode.
- Uploads refuse a file that shrank while being read, `fleet autocomplete install` (bash) no longer deletes your own rc lines on a re-run, and `fleet logs --server` / `--service` filter the audit log on their own.
- Alerts are no longer recorded as notified by a desktop notifier that is disabled, and a pre-release tag can no longer be published to the stable channel.

## v2.6.1

*Released 2026-10-23*

A patch release that lets `fleet server remove` tear an agent down over its own connection, makes `server add` honour your configured default agent port, and rebuilds on Go 1.26.10 for a round of standard-library security fixes.

### Highlights

- **Remove servers without SSH login credentials:** `fleet server remove` now asks the agent, over its existing authenticated connection, to remove itself — running the teardown as a transient systemd unit (`cenvero-fleet-teardown-<token>`) so it outlives the agent's own service. A server whose agent was installed with a password, and whose login key is not authorised for sshd, no longer fails with `ssh: unable to authenticate`. If the agent cannot be reached, remove falls back to SSH as before; `--via-ssh` still means SSH only.
- **`fleet server add` honours `runtime.default_agent_port`** when `--port` is omitted, instead of always using 2222. Interactive add now asks for the agent port and offers that default; the port is range-checked before anything is saved, and auto-install refuses an agent port equal to the login SSH port.
- **Security:** built with Go 1.26.10, which fixes 12 standard-library vulnerabilities reachable from Fleet in go1.26.6 (`net/http`, `net/textproto`, `crypto/tls`, `crypto/x509`, `os`).
- The WinGet submit helper now titles its pull request the way `wingetcreate` and Komac do — `New version: Cenvero.Fleet version X.Y.Z` for a newer release and `Add version:` for an older one.

## v2.6.0

*Released 2026-10-07*

Agents now keep themselves on the controller's version without anyone running a command, agent installs move far less over SSH, and Fleet is in the Windows Package Manager catalog.

### Highlights

- **Automatic agent sync, on by default:** any `fleet` command whose last agent sync started more than an hour ago runs `fleet sync-agent` in the background — the command never waits for it — and a running daemon (`fleet start`) syncs hourly and updates an agent as soon as it connects with an older version. Only agents known to be older than the controller are touched, and runs never overlap. `fleet sync-agent auto` shows the last run and the next; `fleet sync-agent auto off` turns it off. [Docs](https://fleet.cenvero.org/docs/#agent-auto-sync)
- **Lighter agent installs:** the server keeps the release archive it downloads and verifies it with the controller checking the signature, instead of sending the archive back and the binary up again — about 12 MB less over SSH per Linux server.
- **Windows Package Manager:** install with `winget install --id Cenvero.Fleet --exact --source winget`; WinGet then owns upgrades and removal of the controller.
- An agent install's download steps no longer stall until the five-minute deadline on a busy server, and `fleet sync-agent` rejects a stray server name instead of syncing every server (use `--server`).

## v2.5.0

*Released 2026-09-27*

Safe in-place file editing for people and AI agents, a large performance pass, rebuilt terminal and web interfaces, and a round of safety, security and correctness fixes found by an end-to-end review of every command.

### Highlights

- **Edit files in place:** `fleet file view` and `fleet file edit` change a file on the server with an exact-text replace, an insert or new content — no download and re-upload. The file keeps its owner, mode, ACLs and SELinux label, is replaced atomically (a dropped connection never leaves half a file), is only changed if it is still the version you read, and can be undone. A file that can't be replaced by a new copy, such as one bind-mounted into a container, can be written in place with `--in-place`. Fleet saves the current version first, so an interrupted write can always be undone. Built to be safe for AI agents. [Docs](https://fleet.cenvero.org/docs/#file-edit)
- **Much faster at scale:** fleet-wide `exec`, metrics polling, `health`, `top` and agent updates run servers concurrently; CLI calls reuse a running daemon's warm connection (about 550 → 90 ms per command at 50 ms latency); transfers and `fleet sync` are several times faster over latency.
- **Terminal dashboard** rebuilt as a live operations console with sparklines, filters and actions; the **terminal file manager** gains a transfer queue, previews, bookmarks and go-to.
- **Redesigned web UI** with light and dark themes, a phone layout, a command palette, previews, streaming downloads and a read-only **Fleet overview**.
- **`fleet start` / `fleet stop`** run and stop the daemon in the background, and `fleet status` shows whether it is running.
- **`fleet approve`** now shows the staged request, asks for confirmation and runs it with its original options.
- `--secret` values reach the whole remote command through its environment; the audit log records commands, approvals and token and secret changes.
- The daemon's local control socket is **mutually authenticated**, and paths containing `..` are refused everywhere before any file operation.

## v2.4.3

*Released 2026-09-11*

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
