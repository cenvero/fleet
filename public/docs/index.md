# Cenvero Fleet documentation

> Markdown version of <https://fleet.cenvero.org/docs/>. The complete reference in one file is at <https://fleet.cenvero.org/llms-full.txt>.

Cenvero Fleet is a free, open-source, self-hosted fleet manager for Linux, macOS and Windows servers. This guide shows how to install the controller, add your servers, and operate them from the command line, the terminal dashboard or the browser. Every example is a complete command you can copy and run.

## Overview

Cenvero Fleet manages Linux, macOS and Windows servers from one controller that you run yourself. The `fleet` binary on your machine talks to a small `fleet-agent` on each server over an authenticated, host-key-pinned SSH channel. State lives in a local directory; there is no account and no hosted control plane.

**Controller — `fleet`**

The CLI, the terminal dashboard, the terminal and web file managers, and the daemon. Most commands print JSON.

[Set up the controller](https://fleet.cenvero.org/docs/#init)

**Agent — `fleet-agent`**

Runs on each server. Installed for you on Linux with systemd, or started by hand anywhere else.

[Install agents](https://fleet.cenvero.org/docs/#auto-install)

**Direct and reverse modes**

Direct: the controller connects to the agent. Reverse: the agent dials out, so servers behind NAT need no inbound port.

[Choose a mode](https://fleet.cenvero.org/docs/#transport)

**Guard rails for unattended work**

Scoped tokens, named secrets, approvals, command policy and a dead-man's switch for scripts and AI agents.

[Operate safely](https://fleet.cenvero.org/docs/#tokens)

### Quick start

1. **Install the controller** on the machine you work from (other methods are under [Install](https://fleet.cenvero.org/docs/#install)).

   ```sh
   curl -fsSL https://fleet.cenvero.org/install | sh
   ```

2. **Set up the controller.** Creates the config directory, keys and databases.

   ```sh
   fleet init
   ```

3. **Add a Linux server.** Fleet logs in over SSH and installs the agent.

   ```sh
   fleet server add web-01 192.0.2.10 --login-user root
   ```

4. **Run something.** One server, a tag group, or the dashboard.

   ```sh
   fleet exec web-01 uptime
   fleet dashboard
   ```

> These docs follow the **main branch**. The latest stable release is **v2.4.3**; a few commands described here — `fleet start` and `fleet stop` for a background daemon, `fleet version`, and `fleet approve` running the approved command — ship in the next release after it. Check what you have with `fleet --version`, and see [what's new](https://fleet.cenvero.org/whats-new.html).

### Conventions and global flags

Examples use server names such as `web-01` and documentation addresses such as `192.0.2.10`; replace them with your own. For any command, `fleet <command> --help` shows concise help and `fleet ai <command>` prints everything about it. These flags work on every command:

| Flag | What it does |
|---|---|
| `--config-dir` | Use a controller directory other than the default. `FLEET_CONFIG_DIR` does the same. |
| `--token` | Run inside a [scoped RBAC token](https://fleet.cenvero.org/docs/#tokens). Falls back to `FLEET_TOKEN`. |
| `-v`, `--version` | Print the controller version. |
| `-h`, `--help` | Help for the command. |

## Install

The controller is a single signed binary with no runtime dependencies. Install it on the machine you manage servers from — a laptop, a bastion host or a small VM.

**macOS**

Homebrew (recommended):

```sh
brew tap cenvero/fleet && brew install cenvero-fleet
```

or the install script:

```sh
curl -fsSL https://fleet.cenvero.org/install | sh
```

The script installs to `/usr/local/bin/fleet`. Upgrade a Homebrew install with `brew upgrade cenvero-fleet`.

**Linux**

Install script:

```sh
curl -fsSL https://fleet.cenvero.org/install | sh
```

Detects your CPU architecture, verifies the signature and installs `/usr/bin/fleet` (using `sudo` if needed). Homebrew on Linux works too.

**Windows**

PowerShell:

```powershell
irm https://fleet.cenvero.org/install.ps1 | iex
```

Run from an elevated PowerShell prompt (PowerShell 5.1 or later). It fetches a checksum-pinned `minisign` verifier if you have none, installs `fleet.exe` and adds it to `PATH`.

The install script needs `curl` and `tar`, and offers to install `jq` and `minisign` if they are missing — signature verification cannot be skipped.

> **Verified releases.** Every release archive is signed with minisign. The installers and `fleet update apply` check the signature against a pinned public key and the SHA-256 checksum before anything is replaced, and the updater refuses to install an older version.

### Build from source

Building needs Go 1.26. `make build` produces both `fleet` and `fleet-agent`.

```sh
git clone https://github.com/cenvero/fleet
cd fleet
make build
```

### Check the version

```sh
fleet version
fleet version --json
```

### Uninstall

`fleet self-uninstall` removes the binary and the controller's config directory (servers, keys, logs). On a Homebrew install it removes the config directory and prints the `brew uninstall` command instead of deleting the binary. Agents on your servers are left running — remove them first with [`fleet server remove`](https://fleet.cenvero.org/docs/#server-add) if you want them gone.

```sh
fleet self-uninstall --yes
```

## Set up the controller

Run `fleet init` once on the controller machine. Every other command refuses to run until it has, and tells you so.

```sh
fleet init
```

The wizard asks seven questions; press Enter to accept each default.

| Step | Choices (default first) |
|---|---|
| 1. Configuration directory | `~/.cenvero-fleet` on Linux, `~/Library/Application Support/Cenvero Fleet` on macOS, `%LOCALAPPDATA%\Cenvero Fleet` on Windows; or a system-wide or custom path |
| 2. Default transport mode | Reverse, direct, or decide per server |
| 3. Networking | Agent SSH port for direct mode (`2222`); controller listen address for reverse mode (`0.0.0.0:9443`) |
| 4. Cryptography | Ed25519, RSA-4096, or both |
| 5. Updates | Channel `stable` or `beta`; policy notify-only, auto-update or disabled |
| 6. Database | SQLite, PostgreSQL, MySQL or MariaDB |
| 7. Retention and sessions | Job-log retention (`7d`) and session reconnect grace (`10m`) |

Then check the result:

```sh
fleet status
fleet config show
```

### Non-interactive setup

For provisioning scripts, pass the answers as flags:

```sh
fleet init --non-interactive --mode direct --agent-port 2222 --db-backend sqlite
```

| Flag | Default | What it sets |
|---|---|---|
| `--non-interactive` | off | Skip the prompts |
| `--init-config-dir` | platform default | Directory to create |
| `--mode` | `reverse` | Default transport mode |
| `--agent-port` | `2222` | Default agent SSH port for direct mode |
| `--listen-address` | — | Where the daemon accepts reverse agents, e.g. `0.0.0.0:9443` |
| `--crypto` | `ed25519` | Key algorithm: `ed25519`, `rsa-4096` or `both` |
| `--passphrase` | — | Passphrase for the private keys |
| `--channel` | `stable` | Update channel |
| `--policy` | `notify-only` | Update policy |
| `--db-backend` | `sqlite` | `sqlite`, `postgres`, `mysql` or `mariadb` |
| `--db-dsn` | — | DSN for PostgreSQL, MySQL or MariaDB |
| `--alias` | `fleet` | Short alias for the controller binary |

## Add a server

Register servers with `fleet server add`. With no arguments it prompts for the name, address, transport mode and login details.

```sh
# Guided prompts
fleet server add

# A Linux server over SSH: Fleet installs the agent for you
fleet server add web-01 192.0.2.10 --login-user root --login-key ~/.ssh/id_ed25519

# Register only; you start the agent yourself
fleet server add mac-01 192.0.2.20 --no-agent

# Reverse mode: the agent dials out to the controller
fleet server add edge-01 unknown --mode reverse
```

Names must be unique. For reverse mode the address can be a placeholder such as `unknown`; the command prints a one-time token and the controller's fingerprint for the agent (see [reverse mode](https://fleet.cenvero.org/docs/#transport)).

### Flags for `fleet server add`

| Flag | Default | What it does |
|---|---|---|
| `--mode` | `direct` | Transport mode: `direct` or `reverse` |
| `--port` | `2222` | Agent SSH port after install |
| `--user` | `root` | User the agent connection authenticates as |
| `--key` | — | SSH key path override for the agent connection |
| `--login-user` | — | Login user for agent auto-install, e.g. `root` or `ubuntu` |
| `--login-port` | `22` | SSH port used for the install login |
| `--login-key` | — | Private key for the install login |
| `--login-password` | — | Password for the install login (prefer keys) |
| `--sudo` | off | Run install steps with `sudo` when the login user is not root |
| `--no-agent` | off | Register only; skip the agent install |
| `--accept-new-host-key` | off | Re-pin a changed SSH host key without prompting, after you have verified it |

### Inspect and manage servers

```sh
fleet server list
fleet server list --format json
fleet server show web-01
fleet server metrics web-01
fleet server reconnect web-01
fleet server mode edge-01 reverse
fleet server enroll-token edge-01
```

```text
NAME    MODE     ADDRESS          STATUS  NODE    OS/ARCH      VERSION
db-01   direct   192.0.2.30:2222  online  db-01   linux/amd64  v2.4.3
edge-01 reverse  unknown:2222     online  edge-01 linux/arm64  v2.4.3
web-01  direct   192.0.2.10:2222  online  web-01  linux/amd64  v2.4.3
```

`reconnect` connects again and refreshes what the controller knows; add `--accept-new-host-key` after you have verified a changed host key. `enroll-token` mints a fresh one-time token for a reverse agent, for example after it lost its key.

### Remove a server

Removing a server with a managed agent logs in with the stored credentials, stops and removes the systemd service, binary and state directory, and forgets the pinned host key.

```sh
fleet server remove web-01

# The server is gone: delete the record without touching it
fleet server remove web-01 --force

# Credentials changed: retry the teardown with others
fleet server remove web-01 --via-ssh --login-user ubuntu --login-key ~/.ssh/id_ed25519
```

## Agent install (Linux · systemd)

When you add a direct-mode server with `--login-user`, Fleet logs in over SSH and installs the agent. Before anything changes it shows what will be installed and waits for Enter:

```sh
fleet server add web-02 192.0.2.11 --login-user ubuntu --login-port 2200 --sudo
```

```text
The following will be installed on web-02 (192.0.2.11:2200):
  • fleet-agent binary  →  /opt/cenvero-fleet/fleet-agent
  • agent endpoint  →  192.0.2.11:2222
  • cenvero-fleet-agent.service  →  systemd unit (enabled on boot)
  • authorized_keys entry for this controller's public key

To skip, press Ctrl+C now or re-run with --no-agent.
Press Enter to continue:
```

### What happens

1. Fleet opens an SSH connection and pins the server's host key on first contact.
2. It detects the CPU architecture with `uname -m`.
3. The server downloads only its matching `fleet-agent` archive, signature and release manifest (`wget`, falling back to `curl`, with retries).
4. The files are streamed back through the pinned connection, and the controller checks the minisign signature, the version and target, the size and the SHA-256 before installing.
5. The agent is installed to `/opt/cenvero-fleet/fleet-agent`, and `cenvero-fleet-agent.service` is created, enabled and started.

When it finishes, connect with `fleet server reconnect web-02`.

### Changed host keys

If a later install finds a different SSH host key — the machine was rebuilt, or something is intercepting the connection — Fleet does not trust it silently. On a terminal it asks *host key for web-02 has CHANGED … replace and continue? [y/N]*. In scripts, verify the new key out of band and pass `--accept-new-host-key`.

### Retry or adjust with `fleet server bootstrap`

`bootstrap` installs or reinstalls the agent on an existing server record. It uses `sudo` by default.

```sh
# Review the generated install script without running it
fleet server bootstrap web-02 --login-user ubuntu --print-script

# Retry after a failed install, re-pinning a verified new host key
fleet server bootstrap web-02 --login-user ubuntu --accept-new-host-key

# Install a reverse-mode agent that dials the controller
fleet server add edge-02 198.51.100.7 --mode reverse
fleet server bootstrap edge-02 --login-user ubuntu --controller 203.0.113.10:9443
```

Other flags: `--login-port`, `--login-key`, `--listen` (agent address for direct mode), `--service-name`, and `--agent-binary` to upload a local `fleet-agent` build.

### Manual setup (macOS, Windows, or no SSH login)

Download `fleet-agent` for the server's platform from [GitHub Releases](https://github.com/cenvero/fleet/releases), give it the controller's public key, and start it listening on all interfaces:

```sh
# On the controller: export the public key and copy it to the server
fleet key export-pub | jq -r .ed25519 > fleet-controller.pub
scp fleet-controller.pub admin@192.0.2.20:

# On the server: accept that key and listen for the controller
fleet-agent serve --listen 0.0.0.0:2222 --authorized-keys ~/fleet-controller.pub

# Back on the controller
fleet server add mac-01 192.0.2.20 --no-agent
fleet server reconnect mac-01
```

Run the agent under your init system (launchd, a Windows service) so it survives reboots. Service and firewall management are Linux features; on other platforms those calls return a clear "unsupported" error.

## Direct and reverse modes

Each server uses one of two transport modes, and one fleet can mix both. All calls ride a single authenticated `fleet-rpc` SSH channel — public-key authentication only, with no separate unauthenticated port.

|   | Direct | Reverse |
|---|---|---|
| Who connects | Controller → agent | Agent → controller daemon |
| Port | Agent listens on `2222` | Daemon listens on `9443` |
| Use when | The controller can reach the server | The server is behind NAT or a firewall |
| Needs | Nothing running on the controller | The [controller daemon](https://fleet.cenvero.org/docs/#daemon) |
| Interactive `fleet ssh` | Yes | No — use `fleet exec` |

### Direct mode

- The agent runs an SSH server on its fleet port; its host key is pinned in `keys/known_hosts` on first connect, and a changed key is refused.
- A controller process keeps one pooled connection per server and multiplexes calls and file transfers over it. Keepalives every 15 seconds retire a dead connection within about a minute.
- While a daemon is running, one-shot commands reuse its warm connection (one round trip instead of a new handshake). Every RBAC, policy, redaction and audit decision still happens in your CLI first. Set `FLEET_NO_DAEMON_RELAY=1` to always connect directly.

### Reverse mode

Register the server, start the daemon, then start the agent on the server with the token and fingerprint that `fleet server add` printed:

```sh
fleet server add edge-01 unknown --mode reverse
fleet start
```

```sh
# On the server: store the one-time token in an owner-only file
install -m 600 /dev/null /etc/fleet-enroll.token
# ...write the token into /etc/fleet-enroll.token, then:
fleet-agent reverse --controller controller.example.net:9443 --server-name edge-01 \
  --controller-fingerprint SHA256:nu6TQdpWP6p0wyMzzohCi40xbbrGrDOj6rn4HpD+OXA \
  --enroll-token-file /etc/fleet-enroll.token
```

- On first connect the agent checks the controller fingerprint and presents the token; the controller pins the agent's key in `keys/agents/edge-01.pub`. The token is used once and the agent deletes the file.
- The agent reconnects on its own with jittered backoff (1 s to 30 s by default), and after a controller restart within about a second.
- While disconnected it queues metrics snapshots and replays them when it reconnects.
- If it cannot connect it says why on stderr — controller unreachable, fingerprint mismatch, rejected token — with the next retry time.
- With the daemon stopped, commands against reverse servers say so and how to start it.

The daemon must listen on an address the agents can reach — `runtime.listen_address`, which `fleet init` asks for.

### Change a server's mode

```sh
fleet server mode edge-01 reverse
```

### Transport security

- Ed25519 keys by default (RSA-4096 available), and host keys pinned in both directions.
- AEAD ciphers only (`chacha20-poly1305`, `aes256-gcm`) with pinned key-exchange, MAC and host-key algorithms.
- Typed controller-to-agent calls rather than free-form shell strings.

## Tags and groups

Tags are `key=value` labels kept on the controller (in `tags.json`); nothing is written to the servers. Use them to target groups of servers.

```sh
fleet tag web-01 role=web env=prod
fleet tag web-02 role=web env=staging
fleet tag web-01 env=                 # an empty value deletes the tag
fleet tag web-01                      # one server's tags
fleet tag --list                      # every server and its tags
```

### Group expressions

A group expression is one or more `key=value` pairs joined by commas, and every pair must match exactly: `role=web` or `role=web,env=prod`.

```sh
fleet exec --group role=web,env=prod -- uptime
fleet health --group role=web
fleet file diff --group role=web /etc/nginx/nginx.conf
```

Commands that take `--group`: [`exec`](https://fleet.cenvero.org/docs/#exec), [`run`](https://fleet.cenvero.org/docs/#playbooks), [`health`](https://fleet.cenvero.org/docs/#health), [`top`](https://fleet.cenvero.org/docs/#health), [`agent update`](https://fleet.cenvero.org/docs/#agent-updates), [`file diff`](https://fleet.cenvero.org/docs/#files) and [`token create`](https://fleet.cenvero.org/docs/#tokens) (to scope a token). A group that matches no server is an error. The dashboard's server filter also accepts `tag=value`.

## Shell access

`fleet ssh` opens an interactive root shell through the agent, authenticated with the controller's key — there are no separate SSH credentials to manage.

```sh
fleet ssh web-01
```

- The host fingerprint is shown only when it is first pinned; later connects are silent unless the key changes.
- If the network drops, Fleet prints *Connection lost. Reconnecting in 5s... (1/3)* and retries up to three times. The server keeps the session alive for the **reconnect grace** period (default 10 minutes), so you land back in the same shell with the output you missed.
- Typing `exit` ends the session without retrying, and `fleet ssh` exits with the remote shell's exit status, so it works in scripts: `echo 'make test' | fleet ssh web-01`.
- Shells are for direct-mode servers. Reverse-mode servers are refused straight away; use [`fleet exec`](https://fleet.cenvero.org/docs/#exec).

Change the grace period at any time; it applies from the next connection:

```sh
fleet config set session-grace 15m
```

## Run commands

`fleet exec` runs a shell command on one server, on every server with `--all`, or on a [tag group](https://fleet.cenvero.org/docs/#tags) with `--group`.

```sh
fleet exec web-01 uptime
fleet exec web-01 "df -h /"
fleet exec --all -- free -m
fleet exec --group role=web -- systemctl is-active nginx
```

```text
=== web-01 [exit 0] ===
active
=== web-02 [exit 0] ===
active
```

Put `--` before a remote command that has its own flags, so `fleet` does not try to parse them.

### Structured output

Add `--json` for anything a script or agent will parse:

```sh
fleet exec web-01 --json --timeout 10s "df -h /"
```

```text
{"server":"web-01","stdout":"Filesystem      Size  Used Avail Use% Mounted on\n/dev/vda1        40G   12G   26G  32% /\n","stderr":"","exit_code":0,"duration_ms":41,"timed_out":false}
```

Fields: `stdout`, `stderr`, `exit_code`, `duration_ms`, `timed_out` and, on a transport problem, `agent_error`. With `--all` or `--group` you get an array with one entry per target; targets where the command did not run carry `"status"` — `blocked`, `staged`, `dry-run` or `cached` — and an `"error"` when blocked.

### Fan-out

Fan-out runs on up to 16 servers at once; `--parallel N` changes that (`1` runs one server at a time). Output is always printed per server in target order.

```sh
fleet exec --group role=web --parallel 4 -- systemctl reload nginx
fleet exec --all --json -- uptime | jq -r '.[] | "\(.server) \(.exit_code)"'
```

A fan-out exits non-zero when any server failed — blocked by policy, unreachable, timed out or a non-zero remote exit. With `--propagate-exit` the first non-zero remote exit code, in target order, becomes the exit status. In `--json` mode only a policy block makes the exit status non-zero, and notes go to stderr so stdout stays valid JSON.

### Timeouts, retries and repeat runs

```sh
# Abort after 5 minutes; retry only if the transport fails
fleet exec web-01 --timeout 5m --retry 2 --backoff 5s -- /opt/app/migrate.sh

# A retried call with the same key returns the cached result instead of running again
fleet exec web-01 --idempotency-key release-2026-09-26 -- /opt/app/deploy.sh

# Run a recovery command on the same server if this one fails
fleet exec web-01 --on-fail "systemctl restart nginx" -- nginx -s reload

# Print what would run, and where, without running it
fleet exec --group role=web --dry-run -- systemctl reload nginx
```

`--retry` never re-runs a command that actually ran; it only covers connection failures. Idempotency results are cached on the controller for an hour per key, server and command.

### All flags

| Flag | Default | What it does |
|---|---|---|
| `--all` | off | Run on every server |
| `--group` | — | Run on servers whose tags match the expression |
| `--parallel` | `16` | Servers at once for `--all`/`--group` |
| `--json` | off | Structured result |
| `--timeout` | none | Abort after a duration and report `timed_out` |
| `--retry` | `0` | Retry transport failures up to N times |
| `--backoff` | `2s` | Delay between transport retries |
| `--propagate-exit` | off | Exit with the remote command's exit code |
| `--dry-run` | off | Print `would run: …` for each target and stop |
| `--idempotency-key` | — | Return the cached result for this key instead of re-running |
| `--on-fail` | — | Command to run on the same server if this one fails |
| `--secret` | — | Inject `VAR=@name` (a stored secret) or `VAR=literal`; repeatable; values are redacted — see [secrets](https://fleet.cenvero.org/docs/#secrets) |
| `--guard` | off | Refuse commands that could lock the controller out — see [guard](https://fleet.cenvero.org/docs/#guard) |
| `--guard-warn` | off | Downgrade `--guard` to a warning |
| `--confirm` | off | Confirm a command that the [command policy](https://fleet.cenvero.org/docs/#approvals) marks confirm-required |
| `--require-approval` | off | Stage the command for a human to [approve](https://fleet.cenvero.org/docs/#approvals) instead of running it |

## Port tunnels

`fleet tunnel` forwards a local port to a host and port that the *server* can reach — a private database, an internal admin page, another machine on its network. The server makes the outbound connection over the agent transport.

```sh
# localhost:15432 on your machine → 10.0.0.2:5432, reached from web-01
fleet tunnel web-01 15432:10.0.0.2:5432

# Shorthand: web-01's own localhost:80
fleet tunnel web-01 8080:80
```

In another terminal, connect to the local end, for example `psql -h 127.0.0.1 -p 15432 -U app appdb`. The local port binds to `127.0.0.1` only and accepts several connections at once; press Ctrl-C to close the tunnel. A server-scoped [token](https://fleet.cenvero.org/docs/#tokens) may only tunnel to loopback targets on the server.

## File commands

`fleet file` moves and manages files over the same authenticated SSH channel as everything else — no extra port or daemon. Transfers are:

- **Chunked and parallel** — 1920 KiB chunks over several concurrent channels (8 by default) on the pooled connection.
- **Checksummed** — every chunk and the whole file are verified with SHA-256.
- **Resumable** — re-run an interrupted upload or download and only the missing chunks are sent. The destination is replaced atomically.
- **Guarded** — paths with `.` or `..` components are refused for anything that creates, replaces or deletes.

Progress goes to stderr: a live bar on a terminal, otherwise one JSON object per line, so stdout stays clean for scripts.

### Browse and read

```sh
fleet file list web-01 /var/www
fleet file stat web-01 /etc/nginx/nginx.conf
fleet file cat web-01 /etc/hostname
fleet file tail web-01 /var/log/nginx/error.log -n 50 --search timeout
```

### Upload and download

```sh
fleet file upload web-01 ./site.tar.gz /srv/releases/
fleet file upload web-01 ./public /var/www/site -r
fleet file download web-01 /var/log/syslog ./
fleet file download web-01:/etc/nginx ./nginx-config -r
```

If you leave out the remote path (or end it with `/`), an upload lands in the server's default remote directory. Uploading onto an existing directory puts the file inside it. With `-r`, several files move at once.

### Server to server

Copies within one server run on the agent; between servers the bytes stream through the controller without a temporary copy, so it works in every transport mode.

```sh
fleet file copy web-01:/etc/nginx/nginx.conf web-02:/etc/nginx/nginx.conf
fleet cp web-01:/srv/app db-01:/srv/app -r
fleet file move web-01:/srv/old-release db-01:/srv/archive/old-release -r
```

`fleet cp` is a shortcut for `fleet file copy`. `move` renames within one server, and copies then deletes across servers.

### Manage

```sh
fleet file mkdir web-01 /srv/releases/2026-09-26
fleet file mv web-01 /srv/current /srv/previous
fleet file rm web-01 /srv/releases/2026-08-01 --recursive
```

### Archives

Formats are `zip`, `tar.gz`, `tar.bz2`, `tar.xz` and `tar`, taken from the extension or `--format`. Items must sit in the same directory as the archive.

```sh
fleet file compress web-01 /srv/site.tar.gz public index.html
fleet file compress web-01 /var/log/app-logs.zip app.log app.log.1 --format zip
fleet file extract web-01 /srv/releases/site.tar.gz
```

`extract` unpacks into the archive's directory, overwriting files with the same names, and refuses archives with absolute or `..` paths, links or special files.

### Edit and compare

To change a file, edit it in place on the server — see [Edit files in place](https://fleet.cenvero.org/docs/#file-edit).

```sh
# Interactive: opens $EDITOR (then vi, then nano) and saves back only if you changed something
fleet file edit web-01:/etc/nginx/nginx.conf

# Unified diff; exit status 1 when the files differ
fleet file diff web-01:/etc/nginx/nginx.conf web-02:/etc/nginx/nginx.conf
fleet file diff --group role=web /etc/nginx/nginx.conf
```

### Transfer defaults

Each transfer takes its settings from per-server overrides, then global defaults, then the built-in defaults (8 parallel streams, 1920 KiB chunks; larger chunk sizes are capped at that). `--parallel` and `--chunk-size` on a single command override both.

```sh
fleet file defaults show
fleet file defaults show web-01
fleet file defaults set --parallel 8 --remote-dir /srv/incoming
fleet file defaults set web-01 --parallel 2 --remote-dir /data
```

### Confine the agent

By default the agent can read and write any path its user can (except `/proc`, `/sys` and `/dev`). Start it with allowed roots to limit that:

```sh
fleet-agent serve --listen 0.0.0.0:2222 --authorized-keys ~/fleet-controller.pub --file-root /srv/incoming --file-root /var/www
```

> `--file-root` bounds listing, reading, writing, creating, deleting and renaming. Archive, permission and checksum operations run through the agent's shell and are **not** confined by it — restrict the agent's user if you need a hard boundary.

## Edit files in place (Next release)

`fleet file edit` changes a file **on the server** without downloading and re-uploading it: the agent applies the edit and saves it the way a careful editor does. It is designed to be safe to hand to an AI agent such as Claude Code or Codex.

- **Exact edits** — `--old` must match the file exactly (whitespace, indentation and line breaks included) and exactly once unless you pass `--all`, so an edit never lands somewhere you did not intend. Add lines with `--insert-after N --text`, apply several edits all-or-nothing with `--edits`, or replace the whole file with `--content`.
- **Only the version you read** — `fleet file view` prints the file's sha256; pass it as `--expect-sha256` and the edit is refused with `edit_conflict` if the file changed in between, even while the edit is being applied.
- **Permissions kept** — the new content is written to a private temp file beside the original, fsynced, read back and checked against its sha256, and given the original's owner, group, mode (including set-uid/set-gid), POSIX ACLs, SELinux label and other extended attributes before one atomic rename replaces the original. If anything cannot be carried over, nothing changes. Editing through a symlink changes its target and keeps the link.
- **Network drops** — the agent acts only on a request that arrived whole, so a dropped connection changes nothing and nobody ever sees a half-written file. A retry after a lost reply returns the first result instead of editing twice.
- **Undo** — the previous version is kept on the controller; `--undo` restores it, but only while the file is still exactly what that edit produced.

### View, then edit

```sh
# Numbered lines plus the file's sha256 (add --lines 20:60 or --json)
fleet file view web-01 /etc/nginx/nginx.conf

# Replace exact text, only if the file is still the version you viewed
fleet file edit web-01 /etc/nginx/nginx.conf \
    --old 'worker_connections 768;' --new 'worker_connections 2048;' \
    --expect-sha256 <sha256>

# Check the change; undo it if the test fails
fleet exec web-01 "nginx -t && systemctl reload nginx"
fleet file edit web-01 /etc/nginx/nginx.conf --undo
```

### Other kinds of edit

```sh
# Insert lines after line 2 (0 inserts at the top)
fleet file edit web-01 /etc/hosts --insert-after 2 --text '10.0.0.5 db-01'

# Several edits from a JSON list, applied in order, all or nothing; preview first
fleet file edit web-01 /srv/app/.env --edits edits.json --dry-run

# Replace the whole file, or create a new one
fleet file edit web-01 /srv/app/config.yml --content ./config.yml --expect-sha256 <sha256>
fleet file edit web-01 /etc/motd --content - --create --mode 0644 < motd.txt

# What can be undone
fleet file edit web-01 /etc/nginx/nginx.conf --history
```

An edit list looks like `[{"old":"a","new":"b"}, {"old":"x","new":"y","all":true}, {"kind":"insert","line":12,"text":"z"}]`. Every edit prints a unified diff, the old and new sha256, the size, mode and owner (`--json` for a structured result), and is recorded in the audit log. CRLF files keep their line endings. Binary files, hard-linked files and files the agent's user could not write itself are refused. With no flags, `fleet file edit` opens the file in `$EDITOR` and saves it back the same safe way; the editors in both file managers do too.

### Settings

| Command | Default | What it does |
|---|---|---|
| `fleet config set edit-backups 20` | 10 | Versions kept per file for `--undo`, as plain copies in the controller's owner-only config directory (0 turns the history off — use it if the files you edit hold secrets). |
| `fleet config set edit-max-size 2M` | 8M | Largest file that can be viewed or edited (8 MiB at most). |
| `fleet config set edit-require-hash on` | off | Every non-interactive edit of an existing file must pass `--expect-sha256`. |

> In-place editing needs agents with `file.edit` support. Update older agents with `fleet agent update <server>`; until then the interactive editors fall back to the atomic upload, which does not keep the file's owner.

## File manager

Two interactive file managers sit on the same transfer engine: a dual-pane terminal UI and a localhost web UI. Both work local ↔ server and server ↔ server.

### Terminal: `fleet files`

```sh
fleet files                 # Local on the left, the first server on the right
fleet files web-01          # Local and web-01
fleet files web-01 db-01    # two servers side by side
```

`fleet filemanager` and `fleet fm` are aliases. Each pane's source is Local or any server; press `s` or click the pane title to change it. Single-click selects, double-click or Enter opens. Drag an item to the other pane for a *Copy here · Move here · Cancel* menu, or right-click for a context menu. Copies and moves are confirmed with item counts, sizes and name collisions (overwrite, skip or keep both), and the transfer queue shows speed and ETA, with cancel and retry. It fits any terminal from 80×24, and file names and contents can never inject terminal escape sequences.

| Keys | Action |
|---|---|
| `↑` `↓` / `j` `k` | Move |
| `Enter` / `→` | Open folder, or file info |
| `←` / `Backspace` | Parent folder |
| `Tab` | Switch pane |
| `:` / `Ctrl+G` | Go to a path (Tab completes) |
| `f` / `Ctrl+P` | Jump to an item (fuzzy) |
| `'` / `b` | Bookmarks and recent folders / bookmark this folder |
| `=` | Mirror navigation in both panes |
| `Space` / `V` / `Ctrl+A` / `*` | Select and advance / range mode / select all / invert |
| `c` / `m` | Copy / move to the other pane |
| `n` / `N` | New folder / new file |
| `r` / `d` / `D` | Rename / delete (asks first) / duplicate |
| `e` | View or edit with syntax highlighting (`Ctrl+S` saves) |
| `z` / `x` | Compress / extract |
| `p` / `#` / `i` | Permissions (chmod) / SHA-256 checksum / properties |
| `t` | Focus the transfer queue (`x` cancel, `r` retry, `C` clear finished) |
| `/` / `o` / `v` | Filter / cycle sort / list or icon view |
| `P` / `F3` | Preview pane (text, hex for binaries, metadata) |
| `.` | Show hidden files |
| `g` / `Ctrl+R` | Refresh pane / both panes |
| `?` / `q` | Full key reference / quit |

### Browser: `fleet file ui`

```sh
fleet file ui
fleet file ui --addr 127.0.0.1:9000 --open no
```

It prints a URL such as `http://127.0.0.1:9445/?t=…` with a per-run token and, on a terminal, offers to open your browser (`--open auto|yes|no`). `fleet filemanager ui` is the same command.

- Two panes by default, up to six; each is Local or any server. Drag between panes to copy or move, drop files from your desktop to upload, and double-click a text file to edit it.
- A per-pane toolbar and right-click menu cover new folder and file, rename, delete, copy, move, compress, extract, permissions, checksum, duplicate, upload and download, with list and icon views, filtering and sortable columns.
- Keyboard first: arrows and `Enter`, `Backspace` for up, `F6` to switch panes, `F2` to rename, `Ctrl`/`⌘`+`C`/`X` then `V` in the other pane, a command palette on `Ctrl`/`⌘`+`K`, and `?` for every shortcut.
- Light and dark themes, a one-pane phone layout, previews of text and common images, streaming downloads that abort rather than deliver a corrupt file, and smooth scrolling through folders with tens of thousands of entries.
- The **Fleet** tab is a read-only overview of every server — status, mode, OS, CPU, memory and disk, last seen, tags and open alerts — with filters and auto-refresh. *Browse* opens a server in a file pane. Start the UI with `--token` and the overview is checked against that token, like `fleet server list`.

> **Locked down by default.** The web UI binds to loopback only, requires the per-run token on every request, accepts mutations only as same-origin `POST`s, sends a strict Content-Security-Policy and never renders previewed SVG or HTML. Its Local source refuses paths inside the controller's config directory.

## Live sync

`fleet sync` keeps a local directory and a server directory mirrored until you press Ctrl-C. One side is the **writer** (the source of truth) and the other a **replica**.

```sh
# Push: local is the writer, the server is the replica
fleet sync web-01 ./site /var/www/site

# Keep files on the replica that the writer does not have
fleet sync web-01 ./site /var/www/site --no-delete

# Pull: the server is the writer
fleet sync web-01 ./backup /srv/data --from remote
```

```text
Live sync  ./site  →  web-01:/var/www/site   (local is the writer)
mirror (replica extras are deleted) · scan every 1s · press Ctrl-C to stop

✓ initial mirror complete — watching for changes…
↑ index.html
↑ assets/app.css
✗ old-page.html
^C
sync stopped — 2 copied, 1 deleted
```

> By default the replica becomes an **exact mirror**: files on the replica that are not on the writer are deleted. Use `--no-delete` to keep them, and double-check `--from` before syncing into a directory that matters.

| Flag | Default | What it does |
|---|---|---|
| `--from` | `local` | Writer side: `local` (push) or `remote` (pull) |
| `--no-delete` | off | Keep replica files that the writer does not have |
| `--interval` | `1s` | Base re-scan interval |
| `--parallel` | server default | Parallel streams per file |

The writer is copied once, then re-scanned: new or changed files overwrite the replica, several at a time. While nothing changes the scan backs off (up to 8× the interval, at most 5 s) and snaps back on the next change. Sync skips `.git` metadata and does not follow symlinks.

## Services (Linux · systemd)

Track the services you care about on each server, then control them and read their logs. Tracked services appear in the dashboard, and `--critical` marks the ones that matter most.

```sh
fleet service add web-01 nginx.service --log /var/log/nginx/error.log --critical
fleet service list web-01
fleet service restart web-01 nginx.service
fleet service stop web-01 nginx.service
fleet service start web-01 nginx.service
```

### Any unit: `fleet svc`

`fleet svc` works on any systemd unit without tracking it first, and returns structured results.

```sh
fleet svc status web-01 nginx.service --json
fleet svc restart web-01 nginx.service
fleet svc enable web-01 nginx.service
fleet svc disable web-01 nginx.service
```

Actions: `status` (active, enabled and failed state plus recent log lines), `start`, `stop`, `restart`, `enable` and `disable`. On macOS and Windows agents these calls return a typed "unsupported" error rather than pretending to succeed.

## Logs and journal

Read tracked service logs live or from the controller's cache, page the systemd journal, and search the controller's audit log.

### Service logs

```sh
fleet service logs web-01 nginx.service
fleet service logs web-01 nginx.service --follow
fleet service logs web-01 nginx.service --lines 500 --search "upstream timed out"
fleet service logs web-01 nginx.service --export nginx-error.log
fleet service logs web-01 nginx.service --cached
```

Tails read backwards from the end of the file, so they stay fast on multi-gigabyte logs; `--follow` resumes from a cursor, so nothing is lost or repeated and a rotated file is picked up. Everything you read is cached under `logs/_aggregated/`, which `--cached` reads even when the server is offline. The cache is bounded by `runtime.aggregated_log_max_size` (5 MiB), `aggregated_log_max_files` (5) and `aggregated_log_max_age` (7 days).

### systemd journal

```sh
fleet journal web-01 --unit nginx
fleet journal web-01 --unit nginx --since 1h -n 200
fleet journal web-01 --unit nginx --follow --grep error
```

`--unit` is required; `--lines` (`-n`) defaults to 200. `--grep` is a case-insensitive substring match, run on the server where journalctl supports it. `--follow` resumes from journalctl's cursor on each poll.

### Audit log

Every state-changing action is appended to a hash-chained, tamper-evident log at `logs/_audit.log`: remote commands (`exec.run`, with exit codes), file changes, approvals, policy changes, and key, token and secret changes — names only, never secret values. `fleet logs` reads it; with `--server` and `--service` it reads a service log instead.

```sh
fleet logs
fleet logs --search exec.run
fleet logs --server web-01 --service nginx.service --follow
fleet logs --export audit-export.log
```

## Health and metrics

Four views, from a fleet-wide check to a single-server checklist. All of them take `--json` or print tables.

### Fleet health: `fleet health`

```sh
fleet health
fleet health --json
fleet health --group role=web --watch --disk 90 --load 2
```

```text
fleet health — 14:02:11  (disk>85%, load>1.00/CPU)

SERVER  STATUS  DISK%  LOAD/CPU  SWAP   REBOOT  CLOCK  PROBLEMS
db-01   OK      41.0   0.12      2.0G   no      ok     -
web-01  WARN    88.4   0.31      none!  no      ok     no-swap, disk-full
web-02  OK      52.7   0.08      2.0G   no      ok     -

2 healthy, 1 with problems
```

| Problem | Raised when |
|---|---|
| `offline` | The agent cannot be reached or does not answer |
| `no-swap` | No swap is configured |
| `disk-full` | `/` is fuller than `--disk` (default 85%) |
| `reboot` | `/run/reboot-required` exists |
| `clock-skew` | The server clock differs from the controller by more than 5 seconds |
| `high-load` | 1-minute load per CPU exceeds `--load` (default 1.0) |

### Live table: `fleet top`

```sh
fleet top
fleet top --group role=web --interval 5s
fleet top --once
```

CPU, memory, swap, disk and load for every server, refreshed in place (every 2 s by default). `--once` prints one frame and exits.

### One server: `fleet doctor`

```sh
fleet doctor web-01
fleet doctor web-01 --json
```

Checks that the agent answers, its port is listening (skipped for reverse-mode servers), `sshd` is reachable on port 22, the root filesystem is below 90%, swap exists, no reboot is pending and the clock is in sync. Each check reports ok, warn, fail or skip, and the exit status is non-zero when any check fails.

### Inventory: `fleet inventory`

```sh
fleet inventory
fleet inventory --refresh
fleet inventory --json web-01
```

Hostname, public and private IPs, OS and version, architecture, CPU, memory and disk, listening ports, running services, agent port and version, and tags — cached in `data/inventory.json`. `--refresh` probes every server again; `--json` emits a stable schema for planning scripts.

### Metrics

```sh
fleet server metrics web-01
```

The [daemon](https://fleet.cenvero.org/docs/#daemon) also collects metrics every `runtime.metrics_poll_interval` (default 1 minute), from up to 16 servers at once with a 20-second limit each, and feeds [alerts](https://fleet.cenvero.org/docs/#alerts). History is kept for 30 days for the dashboard's sparklines.

## Config drift

Capture the content of important files as a baseline, then check later whether anything changed.

```sh
fleet drift capture web-01 --paths /etc/ssh/sshd_config,/etc/fstab
fleet drift web-01
```

Each path is reported as unchanged, **CHANGED** (with a unified diff) or missing, and the command exits non-zero when anything drifted — so it slots into cron or CI. Detected drift also fires the `drift` [notification](https://fleet.cenvero.org/docs/#notify). Baselines are stored under `baselines/` in the config directory; capture again to accept the current state. To compare one file across servers instead of over time, use [`fleet file diff --group`](https://fleet.cenvero.org/docs/#files).

## Alerts

The [daemon](https://fleet.cenvero.org/docs/#daemon) checks every metrics sample against fixed thresholds and raises an alert when a server crosses one. A failed metrics collection raises an alert too, and fires the `offline` [notification](https://fleet.cenvero.org/docs/#notify).

| Metric | Warning | Critical |
|---|---|---|
| CPU | 80% | 90% |
| Memory | 85% | 95% |
| Disk | 85% | 95% |

```sh
fleet alerts
fleet alerts --server web-01 --severity critical
fleet alerts ack metrics-web-01-disk-warning
fleet alerts suppress metrics-web-01-disk-warning --for 24h
fleet alerts unsuppress metrics-web-01-disk-warning
```

Severities are `info`, `warning` and `critical`. Suppression defaults to 6 hours. New critical alerts can raise a desktop notification (`runtime.desktop_notifications`), with reminders no more often than `runtime.alert_notify_cooldown` (6 hours). The dashboard's Alerts tab has the same actions.

## Notifications

Send fleet events to Slack incoming webhooks or any HTTP webhook. Targets are stored on the controller in `notify.json`.

```sh
fleet notify add slack https://hooks.slack.com/services/T0000/B0000/XXXXXXXX --on offline,online,job-failed
fleet notify add webhook https://ops.example.com/fleet-hook --on destructive,drift
fleet notify add webhook http://10.0.0.5:8080/hook --on offline --allow-internal
fleet notify list
fleet notify test --event offline
fleet notify rm 0
```

| Event | Fires when |
|---|---|
| `offline` | The daemon's metrics collection from a server starts failing |
| `online` | Collection from that server recovers |
| `job-failed` | A [background job](https://fleet.cenvero.org/docs/#jobs) finishes with a non-zero exit |
| `drift` | [`fleet drift`](https://fleet.cenvero.org/docs/#drift) finds a change |
| `destructive` | A destructive command succeeds — for example `server remove`, `file rm`, firewall changes, secret changes, `sync`, `key rotate` or setting tags. The message names the command, server and operator, never other arguments. `exec`, `ssh` and `job run` do not fire it. |

`--on` is required. Adding a target with the same kind and URL replaces its events. Loopback, private and link-local addresses are refused unless you pass `--allow-internal`; cloud metadata addresses stay blocked either way. `fleet notify rm` takes the index from `list` or the exact URL.

## Dashboard

`fleet dashboard` is a live terminal operations console. It refreshes every 5 seconds, shows how old its data is, keeps the last good snapshot if a refresh fails, works with keyboard and mouse, fits terminals from 80×24 up, and respects `NO_COLOR`.

```sh
fleet dashboard
```

| Tab | Shows |
|---|---|
| Overview | Online, degraded and offline counts; alerts by severity and state; fleet resource averages and p95; top CPU, memory and disk servers; recent activity |
| Servers | A sortable, filterable table; `Enter` opens a detail pane with CPU, memory and disk sparklines |
| Services | Tracked services across the fleet |
| Logs | A scrollable, searchable log viewer |
| Alerts | Alerts with severity and state filters and actions |
| Ops | The audit trail |

| Keys | Action |
|---|---|
| `1`–`6`, `Tab`, `←` `→` | Switch tab |
| `j` `k`, `Enter`, `Esc` | Move, open details, go back |
| `/` | Filter (words are ANDed; `tag=value` works) |
| `o` / `O` | Next sort column / reverse (or click a header) |
| `r` / `p` / `+` `-` | Refresh now / pause / change the refresh interval |
| `s` / `f` | SSH shell / file manager on the selected server |
| `c` / `m` | Reconnect (asks first) / collect metrics now |
| `L` / `R` | Follow a live log / restart a service (asks first) |
| `a` / `z` / `u` | Acknowledge / suppress (1h, 6h, 24h, 7d) / unsuppress an alert |
| `v` / `t` | Filter alerts by severity / state |
| `?` / `q` | Key reference / quit |

Actions re-run the same `fleet` binary, so tokens, command policy, host-key pinning and the audit log apply exactly as on the command line.

## Firewall and ports (Linux)

There are two ways to manage a server's firewall. `fleet fw` is agent-safe and works with nftables, iptables, firewalld or ufw. `fleet firewall` and `fleet port` drive ufw directly.

### Agent-safe: `fleet fw`

```sh
fleet fw status web-01
fleet fw allow web-01 443/tcp
fleet fw enable web-01 --safe --undo-after 120
```

`fw` always adds an allow rule for the agent's own port before tightening anything, so the controller cannot lock itself out. `enable --safe` switches to default-drop only after checking the agent stays reachable, and schedules a self-reverting timer (default 60 seconds) that reopens the host if the change cuts the agent off. `--force-i-have-console` skips the lockout refusal — use it only with out-of-band console access.

### ufw: `fleet firewall` and `fleet port`

```sh
fleet firewall status web-01
fleet firewall add web-01 "allow 443/tcp"
fleet firewall add web-01 "allow from 203.0.113.0/24 to any port 22"
fleet port list web-01
fleet port open web-01 8443
fleet port close web-01 8443
fleet firewall enable web-01
fleet firewall disable web-01
```

Rules must match `allow|deny|limit|reject PORT[/tcp|/udp]` or `allow|deny|limit|reject from ADDRESS to any port PORT[/tcp|/udp]`; anything else, such as `reset` or `delete`, is refused.

> `fleet firewall enable` turns ufw on immediately. Allow the agent port (`2222` by default) first, or use `fleet fw enable --safe` or a [guarded change](https://fleet.cenvero.org/docs/#guard) instead.

## Background jobs

A job runs a command in a detached process on the server and captures its output, so it keeps going if your laptop sleeps or the controller disconnects.

```sh
fleet job run web-01 "/opt/app/import.sh --full" --name nightly-import
fleet jobs
fleet job status 1
fleet job logs 1 --follow
fleet job wait 1 --timeout 30m && echo "import finished"
```

- `job run` prints the job's ID (a number); `--name` is a label shown in `fleet jobs`. Add `--json` to `run`, `status`, `wait` or `jobs` for machine-readable records.
- `job wait` exits 0 when the job succeeded and 1 when it failed, so you can chain the next step. `--timeout 0` (the default) waits forever.
- Output is written to an owner-only, unpredictably named log in `/var/tmp` on the server. A failed job fires the `job-failed` [notification](https://fleet.cenvero.org/docs/#notify).
- Finished jobs and their logs are pruned on the controller and the servers after the retention period (default 7 days): `fleet config set job-log-retention 30d`, or `0` to keep them.

## Scheduled jobs

Manage cron entries in a server's crontab without touching the lines you wrote by hand. Fleet wraps each job in `# >>> fleet:NAME >>>` marker comments so it can find it again.

```sh
fleet cron add web-01 --name backup --schedule "0 3 * * *" --cmd "/usr/local/bin/backup.sh"
fleet cron list web-01
fleet cron rm web-01 --name backup
```

`--schedule` is a standard five-field cron expression. Adding a job with an existing name replaces it. Names may contain letters, digits, `.`, `_` and `-`. A server without `crontab` is reported as such.

## Playbooks

`fleet run` applies a YAML playbook of ordered, idempotent steps. For each server, a step whose `check` exits 0 is already satisfied and skipped; otherwise its `apply` runs. With `--on-fail rollback`, a failed step undoes the steps already applied on that server, in reverse order.

```yaml
# web-baseline.yaml
name: web-baseline
hosts: role=web
steps:
  - name: install nginx
    check: command -v nginx
    apply: apt-get install -y nginx
    rollback: apt-get remove -y nginx
  - name: enable nginx
    check: systemctl is-enabled nginx
    apply: systemctl enable --now nginx
    rollback: systemctl disable --now nginx
  - name: allow https
    check: ufw status | grep -q 443/tcp
    apply: ufw allow 443/tcp
    rollback: ufw delete allow 443/tcp
```

```sh
fleet run web-baseline.yaml --dry-run
fleet run web-baseline.yaml --on-fail rollback
fleet run web-baseline.yaml --group role=web,env=staging --on-fail rollback
fleet run web-baseline.yaml web-02
```

Targets come from a server name on the command line, else `--group`, else the playbook's `hosts` expression. `--dry-run` prints the resolved plan without running anything. Every step goes through the [command policy](https://fleet.cenvero.org/docs/#approvals); add `--confirm` for confirm-required steps. Each step is reported as `satisfied`, `applied`, `failed`, `skipped`, `rolledback` or `rollback-skipped` (no rollback command).

## Templates

A template is a TOML file in the `templates/` directory of your config directory. Applying it to a server tracks services (optionally starting, stopping or restarting them) and sets firewall state, open ports and rules.

```toml
# templates/web.toml
name = "web"
description = "Nginx front end"

[[services]]
name = "nginx.service"
log_path = "/var/log/nginx/error.log"
critical = true
action = "restart"

[firewall]
open_ports = [80, 443]
rules = ["allow from 203.0.113.0/24 to any port 22"]
```

```sh
fleet template list
fleet template apply web-01 web.toml
```

A `[firewall]` table may also set `enabled = true`, which turns ufw on *before* ports and rules are applied — make sure the agent port is already allowed.

## Shell integration

### Tab completion

`fleet autocomplete install` writes a completion file your shell loads once (instead of running `fleet` on every new shell). Completion includes your live server names: `fleet exec <Tab>`, `fleet ssh <Tab>` and `fleet file list <Tab>` suggest servers with their address and mode.

```sh
fleet autocomplete install
fleet autocomplete install --shell zsh
```

Supported shells are bash, zsh and fish; the default is your `$SHELL`. `fleet completion` still prints a raw completion script for bash, zsh, fish or PowerShell.

### Stored automations

Keep named shell snippets on the controller and load the latest one into every new terminal.

```sh
fleet automation set deploy --file ./deploy.sh
echo 'alias fl=fleet' | fleet automation set default
fleet automation list
fleet automation get deploy
fleet automation rm deploy
fleet shell-init --install
eval "$(fleet shell-init deploy)"
```

`shell-init` prints a snippet for your shell's rc file that evaluates `fleet automation get NAME` (default `default`); `--install` appends it once. Updating a script with `set` takes effect in the next shell.

## Scoped tokens

A token limits what a `fleet` invocation may do — which servers, which commands, which secrets. Give scripts, CI jobs and AI agents a token instead of your full access. Enforcement happens on the controller and **fails closed**.

```sh
# Read-mostly access for CI: exec only, web servers only
fleet token create --name ci --allow exec --group role=web --read-only-default

# A deploy token for two servers that may use one secret and make destructive changes
fleet token create --name deploy --allow exec,file --servers web-01,web-02 --allow-secret deploy_key --destructive

fleet token list
fleet token revoke 08f253baa4f39be3a2ac27d6c0c294fe
```

The token ID is printed once — store it like a password. Present it with `--token` or `FLEET_TOKEN`:

```sh
fleet --token 08f253baa4f39be3a2ac27d6c0c294fe exec web-01 uptime
FLEET_TOKEN=08f253baa4f39be3a2ac27d6c0c294fe fleet exec web-01 uptime
```

| Flag | Scope |
|---|---|
| `--name` | Required label |
| `--servers` | Only these servers (comma-separated) |
| `--group` | Only servers matching a tag expression |
| `--allow` | Only these top-level commands |
| `--deny` | Never these top-level commands |
| `--allow-secret` | A stored secret the token may inject (repeatable); without it, none |
| `--read-only-default` | Deny commands that change things unless explicitly allowed |
| `--destructive` | Permit destructive operations such as `server remove` or `file rm` |

- A server-scoped token may run only in-scope server commands and a small set of safe local ones. Controller management (`config`, `key`, `backup` …), fan-out reads and cross-server transfers it cannot fully check are denied.
- A scoped token can never create or change tokens, and cannot [approve](https://fleet.cenvero.org/docs/#approvals) staged commands.
- Token IDs are stored hashed in `tokens.json`; `token list` shows a short prefix only. Denied attempts are written to the audit log.

## Secrets and redaction

Store credentials by name on the controller and inject them into commands as environment variables. Values are write-only: no command ever prints them.

```sh
fleet secret set deploy_key --generate 40
printf '%s' "$DB_PASSWORD" | fleet secret set db_password
fleet secret list
fleet secret rotate deploy_key --length 48
fleet secret rm db_password
```

`set` takes the value from stdin (recommended), `--generate N` or `--value` (which lands in your shell history). Secrets are encrypted at rest in `secrets.json` (mode 0600).

```sh
fleet exec web-01 --secret DEPLOY_KEY=@deploy_key -- /opt/app/deploy.sh
```

- `VAR=@name` injects a stored secret; `VAR=literal` injects a literal value. Either way the value is redacted from stdout, stderr, the audit log and the echoed or dry-run command.
- The variable is set for the whole remote command, including pipelines and subshells. Current agents receive it outside the command line, so it never appears in the server's process list.
- A scoped token can inject only the secrets listed with `--allow-secret`.

### Output redaction: `fleet policy`

Add regular expressions whose matches are replaced with `***REDACTED***` in command output. A comma separates patterns.

```sh
fleet policy set redact-pattern 'AKIA[0-9A-Z]{16}'
fleet policy set redact-pattern 'AKIA[0-9A-Z]{16},password=\S+'
fleet policy set redact-defaults on
fleet policy show
fleet policy set redact-pattern ''
```

`set redact-pattern` replaces the whole list (an empty value clears it). `redact-defaults on` also masks common secret-looking values.

## Approvals and command policy

### Command policy

Block dangerous commands outright, or make them require `--confirm`. A pattern matches anywhere in the command, and `*` and `?` work as wildcards inside it — so `mkfs*` also catches `cd /tmp && mkfs.ext4 /dev/sdb`. Matching is case-sensitive.

```sh
fleet cmd-policy set deny 'rm -rf /,mkfs,dd if=*'
fleet cmd-policy set confirm 'reboot,shutdown'
fleet cmd-policy show
fleet exec web-01 --confirm -- reboot
```

Each `set` replaces that list; `fleet cmd-policy set deny ''` clears it. The policy is enforced by `fleet exec` and `fleet run`, and stored in `cmd-policy.json`.

### Approvals

Stage a command for a human to review instead of running it. It keeps all its exec options and expires after an hour if nobody acts.

```sh
fleet exec web-01 --require-approval --timeout 2m -- systemctl restart postgresql
```

```text
staged approval 70ea09cc74284eae for web-01 — run: fleet approve 70ea09cc74284eae
```

```sh
fleet approvals list
fleet approve 70ea09cc74284eae
# ...or turn it down
fleet approvals reject 70ea09cc74284eae
```

- `fleet approve` shows the full request — server, command, every option, who staged it and when — and asks for confirmation. Without a terminal, pass `--yes` after reviewing `fleet approvals list --json`.
- The command then runs once through the normal `fleet exec` path, so command policy, guard checks, redaction, audit and token scopes apply again. The outcome (executed or failed, with the exit code) is recorded and shown by `approvals list`; `approve --json` prints the exec result.
- Only registered servers can be staged, and secrets must be `VAR=@name` references — literal values are refused, so no secret is written to `approvals.json`.
- A scoped token cannot approve.

## Dead-man's switch

`fleet guard` runs a risky change and arms a timer *on the server* that runs your undo command unless you confirm in time. Because the timer lives on the server, even a change that cuts you off — a firewall rule, an sshd edit, a network change — reverts on its own.

```sh
fleet guard web-01 "ufw default deny incoming && ufw reload" \
  --revert-after 2m \
  --revert-cmd "ufw default allow incoming && ufw reload"
```

The command prints the change ID, such as `web-01-1`. Check that everything still works, then:

```sh
# Keep the change (cancels the revert)
fleet confirm web-01-1
# ...or undo it now
fleet revert web-01-1
```

`--revert-after` is required. Always give a `--revert-cmd` that fully undoes the change.

### Lighter checks

`fleet exec --guard` refuses commands that could lock the controller out of a server; `--guard-warn` turns that into a warning. For firewalls specifically, [`fleet fw enable --safe`](https://fleet.cenvero.org/docs/#firewall) keeps the agent port open and reverts itself.

```sh
fleet exec web-01 --guard -- "iptables -P INPUT DROP"
```

## Keys

The controller authenticates with its own key pair — Ed25519 by default, or RSA-4096 or both when chosen at [setup](https://fleet.cenvero.org/docs/#init). Keys live in the `keys/` directory.

```sh
fleet key fingerprint
fleet key export-pub
fleet key audit
fleet key rotate
```

`fleet key rotate` generates a new key and rolls it out to every server, direct and reverse, without locking any out:

- **Direct:** the new public key is added to the agent's authorized keys, a live connection with the new key is tested, and only then is the old key removed.
- **Reverse:** the agent is told to trust the new controller key as well, the controller switches, the session reconnects under the new key (waiting up to 45 seconds), and the old key is removed from the agent.

The result lists `rotated_servers` and `verified_servers`. A server in the first list but not the second was rolled back to the old key; a fully successful rotation has identical lists. Old key material is archived under `keys/rotations/`.

## Controller daemon

The daemon accepts [reverse-mode](https://fleet.cenvero.org/docs/#transport) agents, serves the local control socket that other `fleet` processes use to reach them, polls metrics and raises [alerts](https://fleet.cenvero.org/docs/#alerts), and checks for updates. Direct-mode servers work without it, but scheduled metrics polling — and with it alerts and the `offline` and `online` notifications — needs it.

```sh
fleet start       # run it in the background
fleet status      # "daemon": {"running": true, "pid": ...}
fleet stop        # stop it
fleet daemon      # or run it in the foreground
```

- `fleet start` launches `fleet daemon` detached from the terminal, appends its output to `logs/daemon.log`, and waits up to 10 seconds until it accepts connections. If it exits during start-up — a port already in use, say — the command fails and prints the last lines of the log. If a daemon is already running it says so and exits 0. Your `--token` is not passed on.
- `fleet stop` sends SIGTERM (on Windows it terminates the process) and waits up to 15 seconds. It only signals the process that holds the config directory's daemon lock, so a stale pid file never hits an unrelated process.
- `fleet daemon` runs in the foreground — use it under systemd, launchd or a container. However it was started, a daemon records its pid in `data/daemon.pid`, holds `data/daemon.lock` so a second one for the same config directory is refused, and exits cleanly on Ctrl-C or SIGTERM.

> `fleet start` and `fleet stop` manage a background daemon since the next release after v2.4.3. On v2.4.3, run `fleet daemon` under a service manager instead.

| Setting | Default | Purpose |
|---|---|---|
| `runtime.listen_address` | `0.0.0.0:9443` (set at init) | Where reverse agents connect |
| `runtime.control_address` | `127.0.0.1:9444` | Loopback control socket for other `fleet` processes |
| `runtime.metrics_poll_interval` | `1m` | How often metrics are collected |

### The control socket

The control socket is mutually authenticated. The daemon writes a per-run secret to `data/control.token` (owner-only); on every connection it first proves it holds the secret with an HMAC-SHA256 challenge over fresh nonces, and only then does the caller send its request, authenticated the same way. Something else listening on the port while the daemon is down learns nothing. The token file is removed when the daemon stops. One-shot direct-mode commands also relay through the daemon's warm connections unless `FLEET_NO_DAEMON_RELAY=1` is set.

## Configuration

Settings live in `config.toml` in the config directory. Inspect and change them with `fleet config`:

```sh
fleet config show
fleet config validate
fleet config edit
fleet config set session-grace 15m
fleet config set job-log-retention 30d
fleet config agent-port 2222
```

`config edit` opens `$EDITOR` and saves only if the result is valid. `config set` accepts `session-grace` and `job-log-retention` (`7d`, `30d`, `12h`, or `0`/`off`/`never` to stop pruning). `config agent-port` sets the default agent port for new installs.

### Useful settings

| Key | Default | What it controls |
|---|---|---|
| `default_transport_mode` | set at init | `direct`, `reverse` or `per-server` |
| `updates.channel` / `updates.policy` | `stable` / `notify-only` | [Update settings](https://fleet.cenvero.org/docs/#updates) |
| `crypto.algorithm` | `ed25519` | Controller key type |
| `database.backend` | `sqlite` | Where controller state is stored |
| `runtime.session_reconnect_grace` | `10m` | How long a dropped shell stays alive on the server |
| `runtime.job_log_retention` | `7d` | When finished job logs are pruned |
| `runtime.alert_notify_cooldown` | `6h` | Minimum gap between reminders for the same alert |
| `runtime.desktop_notifications` | `true` | Desktop notifications for new critical alerts |
| `runtime.aggregated_log_max_size` / `_max_files` / `_max_age` | 5 MiB / 5 / 7 days | Bounds of the cached service logs |
| `runtime.file_transfer.parallel_streams` | `8` | Global transfer parallelism (`fleet file defaults`) |

### Which config directory is used

In order: the `--config-dir` flag, the `FLEET_CONFIG_DIR` environment variable, the directory chosen at `fleet init`, then the platform default.

```sh
fleet --config-dir /etc/cenvero-fleet status
export FLEET_CONFIG_DIR=/etc/cenvero-fleet
```

### Database backends

SQLite is the default, split into `state.db`, `metrics.db` and `events.db` (WAL mode). PostgreSQL, MySQL and MariaDB are supported too. `database shift` copies the data first and switches the config only once the new backend is ready.

```sh
fleet database show
fleet database shift --backend postgres --dsn 'postgres://fleet:change-me@db.internal:5432/fleet?sslmode=require'
```

### Export and import

A JSON export of the controller state, as an alternative to a tarball backup:

```sh
fleet config export > fleet-state.json
fleet config import fleet-state.json
```

## Config migrations

When an upgrade adds or removes setup options, your `config.toml` may lack new settings or carry old ones. Fleet notices on every command and prints a hint:

```text
⚠  Your fleet config (init_version=2) is behind this version (init_version=3).
   Run 'fleet adjust-init' to review and apply configuration changes.
```

```sh
fleet adjust-init
```

Each pending change is shown as `[✕] removed` (with the reason; the entry is cleaned up) or `[+] added` (you choose a value). The config is saved and stamped with the current `init_version`. It is safe to run at any time.

## Backup and recovery

```sh
# Archive the whole config directory into the current directory
fleet backup
fleet backup --output /backups/fleet-$(date +%Y%m%d).tar.gz

# Or into the config directory's own backups/ folder
fleet config backup

# Restore from an archive
fleet config restore /backups/fleet-20260926.tar.gz
```

The archive holds server records, keys, audit logs and databases; lock files, WAL journals and temporary files are left out.

> A backup contains the controller's **private keys** and its secrets store. Keep it somewhere as protected as the controller itself.

### After a reinstall or a move

Point a fresh installation at an existing config directory:

```sh
fleet recover --from-dir /mnt/old-disk/.cenvero-fleet
```

`recover` checks that the SQLite files exist or that the PostgreSQL, MySQL or MariaDB DSN connects, that the config is valid, and that your `fleet` version matches the one that last used it — if not, it tells you which version to install first. It then prints the `--config-dir` flag and `FLEET_CONFIG_DIR` export to use. `--db-backend` and `--db-dsn` override the stored database settings; `--skip-version-check` is available but not recommended.

## Updates

Fleet updates itself, but only when you say so: the default policy is `notify-only` on the `stable` channel.

```sh
fleet update check
fleet update apply
fleet update rollback
fleet update channel beta
```

- `update apply` updates the controller, then rolls the update out to managed agents and reports any that fail. `--server` (repeatable) limits the rollout.
- `update rollback` restores the binary saved by the last `apply`.
- Channels are `stable` and `beta`.
- Fleet checks the channel at most every 10 minutes and caches the result; when a newer release exists, `fleet` commands print an *Update your fleet* notice on stderr with the right upgrade command for how you installed it.

| Policy | What happens |
|---|---|
| `notify-only` | Default. Tells you about new releases; you apply them. |
| `auto-update` | Also updates version-mismatched managed Linux agents automatically. The controller itself is still updated only when you run `fleet update apply`. |
| `disabled` | No update checks. |

### Verification

Signature checks fail closed on every channel: an update without a minisign signature is refused, and a checksum alone is never enough. Downloads are HTTPS-only with size and decompression limits. The updater refuses anything older than the running version or below the channel's minimum supported version. Two explicit overrides exist: `--allow-downgrade` for a deliberate downgrade, and `--allow-unsigned` for a local build (insecure).

### Homebrew installs

Homebrew owns the controller binary, so upgrade it with `brew update && brew upgrade cenvero-fleet` and update agents with [`fleet sync-agent`](https://fleet.cenvero.org/docs/#agent-updates). There, `fleet update check` works, `update apply` only prints those instructions, and `channel` and `rollback` are blocked.

## Agent updates

Check which agent versions are running, then roll out updates with a canary.

```sh
fleet agent version
fleet agent version web-01
fleet agent update --canary 1
fleet agent update --group role=web --canary 2 --strict-health
```

- `agent version` shows each agent's version in one form (`v2.4.3`), `dev` for development builds and `-` when unknown, and flags mismatches against the controller's version.
- `agent update` updates a first batch of `--canary N` servers (default 1), waits up to 90 seconds for each to reconnect, answer and report the new version, and only then continues. If a canary fails, the rollout stops before touching the rest. `--canary 0` updates everything at once.
- Host problems on a canary (no swap, high load, full disk, pending reboot, clock skew) are reported but do not stop the rollout unless you pass `--strict-health`.

### Match the controller: `fleet sync-agent`

```sh
fleet sync-agent
fleet sync-agent --server web-01 --server web-02
```

Brings agents up to the controller's version, several servers at a time, with progress per server on stderr (*updated*, *up to date* or an error) and JSON on stdout. Servers already on the right version are skipped. Linux agents are restarted through systemd automatically; on Windows the new binary is delivered, but you restart the service and reconnect yourself.

## Agentic Fleet

AI coding agents such as Claude Code and Codex can operate your fleet through the same CLI you use, on your machine and with your keys. One command installs a small skill that tells the agent to read `fleet context` first.

```sh
fleet skill claude     # ~/.claude/skills/cenvero-fleet/SKILL.md and a /fleet command
fleet skill codex      # ~/.codex/prompts/fleet.md
fleet skill agents     # AGENTS.md in the current directory
fleet skill list
```

Restart Claude Code and type `/fleet`. Re-running an install overwrites the files; `--dir` changes where they go and `--print` prints them instead.

### Self-describing CLI

```sh
fleet context             # concepts, safety guidance and every command, as markdown
fleet context --json      # the command tree as JSON
fleet ai file upload      # everything about one command
fleet ai sync --json
```

Both are generated from the installed binary, so they always match your version.

### Guard rails

- Run the agent with a [scoped token](https://fleet.cenvero.org/docs/#tokens) so it can only reach the servers and commands you allow.
- Keep credentials in [secrets](https://fleet.cenvero.org/docs/#secrets) and reference them as `--secret VAR=@name`.
- Use [`fleet guard`](https://fleet.cenvero.org/docs/#guard) for risky changes, [`--require-approval`](https://fleet.cenvero.org/docs/#approvals) when a human must sign off, and `--dry-run` to preview.
- The context tells agents to confirm with you before destructive commands such as `server remove`, `file rm`, `key rotate`, `update apply`, `self-uninstall` and `config restore`.

[Read more about Agentic Fleet](https://fleet.cenvero.org/agentic.html).

## Command reference

All 62 top-level commands and their subcommands. Run `fleet ai <command>` for the full help of any of them, or `fleet context` for everything at once. `fleet help` and `fleet completion` are also available.

| Command | Subcommands | What it does |
|---|---|---|
| [`fleet adjust-init`](https://fleet.cenvero.org/docs/#adjust-init) | — | Apply pending config migrations after a fleet upgrade |
| [`fleet agent`](https://fleet.cenvero.org/docs/#agent-updates) | `update` `version` | Report agent versions and roll out agent updates behind a health-gated canary |
| [`fleet ai`](https://fleet.cenvero.org/docs/#agentic) | — | Print full machine-readable help for a command (for AI agents) |
| [`fleet alerts`](https://fleet.cenvero.org/docs/#alerts) | `ack` `suppress` `unsuppress` | List, acknowledge and suppress alerts |
| [`fleet approvals`](https://fleet.cenvero.org/docs/#approvals) | `list` `reject` | List and reject commands staged with exec --require-approval |
| [`fleet approve`](https://fleet.cenvero.org/docs/#approvals) | — | Review a pending command approval, then approve and run it |
| [`fleet autocomplete`](https://fleet.cenvero.org/docs/#shell) | `install` | Enable shell tab-completion for fleet (cached, no per-shell fleet fork) |
| [`fleet automation`](https://fleet.cenvero.org/docs/#shell) | `get` `list` `rm` `set` | Store named shell scripts; load the latest into every shell via 'fleet shell-init' |
| [`fleet backup`](https://fleet.cenvero.org/docs/#backup) | — | Back up the fleet config directory to a tar.gz archive |
| [`fleet cmd-policy`](https://fleet.cenvero.org/docs/#approvals) | `set` `show` | Manage the dangerous-command policy (deny / confirm patterns) |
| [`fleet config`](https://fleet.cenvero.org/docs/#config) | `agent-port` `backup` `edit` `export` `import` `restore` `set` `show` `validate` | Show, validate, edit and change controller settings; back up, restore, export and import |
| [`fleet confirm`](https://fleet.cenvero.org/docs/#guard) | — | Confirm a guarded change so its dead-man's-switch does not revert |
| [`fleet context`](https://fleet.cenvero.org/docs/#agentic) | — | Print the full agent-facing reference (commands, concepts, workflows) |
| [`fleet cp`](https://fleet.cenvero.org/docs/#files) | — | Copy a file (or directory with -r) directly between two servers |
| [`fleet cron`](https://fleet.cenvero.org/docs/#cron) | `add` `list` `rm` | Add, list and remove managed cron jobs on a server |
| [`fleet daemon`](https://fleet.cenvero.org/docs/#daemon) | — | Run the controller daemon in the foreground (reverse agents, control socket, metrics) |
| [`fleet dashboard`](https://fleet.cenvero.org/docs/#dashboard) | — | Launch the terminal dashboard |
| [`fleet database`](https://fleet.cenvero.org/docs/#config) | `shift` `show` | Show the database backend or move to SQLite, PostgreSQL, MySQL or MariaDB |
| [`fleet doctor`](https://fleet.cenvero.org/docs/#health) | — | Run a health checklist against a server (agent, ports, disk, swap, reboot, clock) |
| [`fleet drift`](https://fleet.cenvero.org/docs/#drift) | `capture` | Capture a baseline of config files and report drift against it |
| [`fleet exec`](https://fleet.cenvero.org/docs/#exec) | — | Run a shell command on one server or all servers |
| [`fleet file`](https://fleet.cenvero.org/docs/#files) | `cat` `compress` `copy` `defaults` `defaults set` `defaults show` `diff` `download` `edit` `extract` `list` `mkdir` `move` `mv` `rm` `stat` `tail` `ui` `upload` | Browse, transfer, edit, compare and archive files on servers |
| [`fleet filemanager`](https://fleet.cenvero.org/docs/#file-manager) | `ui` | Alias of fleet files (also fm); filemanager ui opens the web UI |
| [`fleet files`](https://fleet.cenvero.org/docs/#file-manager) | — | Dual-pane terminal file manager (local and server panes) |
| [`fleet firewall`](https://fleet.cenvero.org/docs/#firewall) | `add` `disable` `enable` `status` | Show, enable and disable ufw and add rules |
| [`fleet fw`](https://fleet.cenvero.org/docs/#firewall) | `allow` `enable` `status` | Agent-safe firewall control that never locks out the agent |
| [`fleet guard`](https://fleet.cenvero.org/docs/#guard) | — | Run a risky command with a dead-man's-switch that auto-reverts unless confirmed |
| [`fleet health`](https://fleet.cenvero.org/docs/#health) | — | Per-server health checks across the fleet (offline, swap, disk, reboot, clock, load) |
| [`fleet init`](https://fleet.cenvero.org/docs/#init) | — | Run the first-time setup flow |
| [`fleet inventory`](https://fleet.cenvero.org/docs/#health) | — | Machine-readable inventory of the fleet (hostname, IPs, OS, resources, ports, services, tags) |
| [`fleet job`](https://fleet.cenvero.org/docs/#jobs) | `logs` `run` `status` `wait` | Run detached background jobs and check, wait for or follow them |
| [`fleet jobs`](https://fleet.cenvero.org/docs/#jobs) | — | List tracked background jobs |
| [`fleet journal`](https://fleet.cenvero.org/docs/#logs) | — | Page or follow the systemd journal for a unit |
| [`fleet key`](https://fleet.cenvero.org/docs/#keys) | `audit` `export-pub` `fingerprint` `rotate` | Show, export, audit and rotate controller keys |
| [`fleet logs`](https://fleet.cenvero.org/docs/#logs) | — | Read the audit log, or a service log with --server and --service |
| [`fleet notify`](https://fleet.cenvero.org/docs/#notify) | `add` `list` `rm` `test` | Send fleet events to Slack or webhooks |
| [`fleet policy`](https://fleet.cenvero.org/docs/#secrets) | `set` `show` | Output redaction patterns for command output |
| [`fleet port`](https://fleet.cenvero.org/docs/#firewall) | `close` `list` `open` | List, open and close ports through ufw |
| [`fleet recover`](https://fleet.cenvero.org/docs/#backup) | — | Re-attach fleet to an existing config directory after a reinstall or migration |
| `fleet report` | — | Show where to report bugs and get support |
| [`fleet revert`](https://fleet.cenvero.org/docs/#guard) | — | Revert a guarded change immediately |
| [`fleet run`](https://fleet.cenvero.org/docs/#playbooks) | — | Run an idempotent playbook against one or more servers |
| [`fleet secret`](https://fleet.cenvero.org/docs/#secrets) | `list` `rm` `rotate` `set` | Store named secrets; values are never printed |
| [`fleet self-uninstall`](https://fleet.cenvero.org/docs/#install) | — | Remove fleet binary and config directory |
| [`fleet server`](https://fleet.cenvero.org/docs/#server-add) | `add` `bootstrap` `enroll-token` `list` `metrics` `mode` `reconnect` `remove` `show` | Add, inspect, reconnect, bootstrap and remove servers |
| [`fleet service`](https://fleet.cenvero.org/docs/#services) | `add` `list` `logs` `restart` `start` `stop` | Track services, control them and read their logs |
| [`fleet shell-init`](https://fleet.cenvero.org/docs/#shell) | — | Print/install a shell snippet that loads the latest automation in every new shell |
| [`fleet skill`](https://fleet.cenvero.org/docs/#agentic) | `agents` `claude` `codex` `list` | Install the Fleet skill for Claude Code, Codex or AGENTS.md |
| [`fleet ssh`](https://fleet.cenvero.org/docs/#ssh) | — | Open an interactive root shell on a server |
| [`fleet start`](https://fleet.cenvero.org/docs/#daemon) | — | Start the controller daemon in the background |
| [`fleet status`](https://fleet.cenvero.org/docs/#daemon) | — | Show controller status, including whether the daemon is running |
| [`fleet stop`](https://fleet.cenvero.org/docs/#daemon) | — | Stop the background controller daemon |
| [`fleet svc`](https://fleet.cenvero.org/docs/#services) | `disable` `enable` `restart` `start` `status` `stop` | Structured systemd control for any unit |
| [`fleet sync`](https://fleet.cenvero.org/docs/#sync) | — | Live mirror a directory between local and a server (writer → replica) |
| [`fleet sync-agent`](https://fleet.cenvero.org/docs/#agent-updates) | — | Bring every agent up to the controller version |
| [`fleet tag`](https://fleet.cenvero.org/docs/#tags) | — | Tag servers with key=value labels and group them |
| [`fleet template`](https://fleet.cenvero.org/docs/#templates) | `apply` `list` | List templates and apply one to a server |
| [`fleet token`](https://fleet.cenvero.org/docs/#tokens) | `create` `list` `revoke` | Create, list and revoke scoped RBAC tokens |
| [`fleet top`](https://fleet.cenvero.org/docs/#health) | — | Live CPU/mem/swap/disk/load table across servers |
| [`fleet tunnel`](https://fleet.cenvero.org/docs/#tunnel) | — | Forward a local port to a host:port reachable by the server |
| [`fleet update`](https://fleet.cenvero.org/docs/#updates) | `apply` `channel` `check` `rollback` | Check for, apply and roll back controller updates; set the channel |
| [`fleet version`](https://fleet.cenvero.org/docs/#install) | — | Print the fleet controller version |

## Directory layout

The default config directory is `~/.cenvero-fleet` on Linux (see [setup](https://fleet.cenvero.org/docs/#init) for macOS and Windows). A typical layout:

```text
~/.cenvero-fleet/
├── config.toml
├── instance.id
├── keys/
│   ├── id_ed25519
│   ├── id_ed25519.pub
│   ├── known_hosts
│   ├── agents/            ← pinned public keys for reverse-mode agents
│   └── rotations/         ← archived key material from past rotations
├── servers/
├── templates/
├── logs/
│   ├── _aggregated/
│   ├── _audit.log
│   └── _audit.log.lock    ← cross-process lock so concurrent fleet processes never fork the audit hash chain
├── alerts/
├── data/
│   ├── state.db
│   ├── metrics.db
│   ├── events.db
│   ├── update-available.json ← cached update check (a failed check is also recorded, so an offline controller retries at most every 10 minutes)
│   └── control.token      ← per-run secret for the daemon's mutually authenticated control socket (removed when the daemon stops)
├── approvals.json         ← staged `exec --require-approval` commands and their outcomes
├── approvals-extra.json   ← copy of the exec options/outcomes, so older fleet binaries rewriting approvals.json cannot drop them
├── tui/
│   └── files-bookmarks.json ← file manager bookmarks
├── backups/
└── tmp/
```

Databases open on first use, so commands that never need them stay fast. Other files appear as you use the features that own them:

| Path | Written by |
|---|---|
| `tags.json` | [`fleet tag`](https://fleet.cenvero.org/docs/#tags) |
| `tokens.json` | [`fleet token`](https://fleet.cenvero.org/docs/#tokens) (IDs hashed) |
| `secrets.json` | [`fleet secret`](https://fleet.cenvero.org/docs/#secrets) (encrypted, 0600) |
| `policy.json`, `cmd-policy.json` | [`fleet policy`](https://fleet.cenvero.org/docs/#secrets), [`fleet cmd-policy`](https://fleet.cenvero.org/docs/#approvals) |
| `notify.json` | [`fleet notify`](https://fleet.cenvero.org/docs/#notify) |
| `guards.json` | [`fleet guard`](https://fleet.cenvero.org/docs/#guard) |
| `jobs.json` | [`fleet job`](https://fleet.cenvero.org/docs/#jobs) |
| `baselines/` | [`fleet drift capture`](https://fleet.cenvero.org/docs/#drift) |
| `automations/` | [`fleet automation`](https://fleet.cenvero.org/docs/#shell) |
| `data/inventory.json` | [`fleet inventory`](https://fleet.cenvero.org/docs/#health) |
| `data/idempotency.json` | [`fleet exec --idempotency-key`](https://fleet.cenvero.org/docs/#exec) |
| `data/daemon.pid`, `data/daemon.lock`, `logs/daemon.log` | [The daemon](https://fleet.cenvero.org/docs/#daemon) |
| `data/update-rollback.json` | [`fleet update apply`](https://fleet.cenvero.org/docs/#updates) |

On a managed Linux server the agent lives in `/opt/cenvero-fleet/` and runs as `cenvero-fleet-agent.service`.
