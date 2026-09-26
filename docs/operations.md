# Operations Guide

This guide covers the main day-to-day operator workflows in Cenvero Fleet.

## Controller Daemon

The controller daemon accepts reverse-mode agents, serves the loopback control socket that
one-shot CLI commands use to reach them (and to reuse warm direct-mode connections), polls
metrics and keeps the update check warm. Run it in the background or in the foreground:

```bash
fleet start     # launch it detached; waits until it is listening, then prints its pid
fleet status    # "daemon": {"running": true, "pid": 12345, ...}
fleet stop      # SIGTERM (Windows: terminate), wait up to 15s for it to exit
fleet daemon    # run it in the foreground (for systemd, launchd, a container, or debugging)
```

- `fleet start` runs `fleet --config-dir <dir> daemon` in its own session with no terminal,
  appends its output to `<config-dir>/logs/daemon.log` (`0600`), and waits up to 10 seconds for
  it to accept connections on `runtime.control_address`. If the daemon exits during start-up
  (for example because its port is taken) the command fails and prints the last lines of the
  log. When a daemon is already running for the config dir it says so and exits 0. Your
  `--token` / `FLEET_TOKEN` is not passed on to the daemon.
- Every daemon, however it was started, records its pid in `<config-dir>/data/daemon.pid` and
  holds a lock on `<config-dir>/data/daemon.lock` while it runs, so a second `fleet daemon` for
  the same config dir is refused with a clear error, and a daemon started by a service manager
  is visible to `fleet status` and stoppable with `fleet stop`.
- `fleet stop` only signals the process that still holds the config dir's daemon lock (on Linux
  it also checks `/proc/<pid>/cmdline`), so a stale pid file never leads to killing an unrelated
  process; a stale pid file is removed and the command reports `fleet daemon is not running`.
- The daemon exits cleanly on SIGINT (Ctrl-C) and SIGTERM, removing its pid file and
  `data/control.token`. It prints its "listening on ..." lines only once its listeners are bound.
- A daemon started by a release older than this one records no pid file: `fleet status` still
  reports it as running when its control address answers, but `fleet stop` asks you to stop it
  yourself.

## Servers

Add, inspect, reconnect, or remove servers:

```bash
fleet server add web-01 192.0.2.10 --mode direct
fleet server list
fleet server show web-01
fleet server reconnect web-01
fleet server remove web-01
```

For Linux-first remote bootstrap with automatic agent installation:

```bash
fleet server add web-01 192.0.2.10 --mode direct --login-user root --login-key ~/.ssh/id_ed25519
```

This SSHes into the server, detects its architecture, and makes the target download only the matching `fleet-agent` archive, signature, and release manifest (`wget` first, with `curl` fallback and retries). The payloads are streamed through the pinned SSH connection so the controller verifies minisign, trusted version/target binding, size, and SHA-256 before it installs the extracted binary under `/opt/cenvero-fleet/` and starts it as a systemd service. The controller machine does not download agent archives from GitHub during auto-install.

**Changed bootstrap host key.** The bootstrap SSH host key is pinned on first install. If a
later install finds a *different* key (the box was reinstalled or re-keyed — or, rarely, a
man-in-the-middle), the install does not silently trust it. On a terminal you are prompted —
`host key for X has CHANGED (pinned A → presented B); replace and continue? [y/N]` (default
No) — and answering yes re-pins the new key in `bootstrap_known_hosts` and continues. For
non-interactive use, pass `--accept-new-host-key` (after verifying the new key out of band):

```bash
fleet server add web-01 192.0.2.10 --login-user root --accept-new-host-key
fleet server bootstrap web-01 --login-user root --accept-new-host-key   # retry an existing server
```

Removing a server with a managed agent tears it down on the remote host and removes the stored host-key entry:

```bash
fleet server remove web-01
```

To remove the local record without SSH-ing to the server (e.g. the server is gone):

```bash
fleet server remove web-01 --force
```

Reverse-mode servers should be registered before the agent dials in:

```bash
fleet server add edge-01 unknown --mode reverse
```

## Shell Access

Open an interactive shell on any managed server:

```bash
fleet ssh web-01
```

This connects through the fleet agent using the fleet controller key — no need to manage separate SSH credentials. The host fingerprint is shown only the first time it is pinned; subsequent connects to the same server print nothing unless the key has changed.

The shell session is persistent: if the network drops mid-session, fleet prints a reconnect notice and retries automatically:

```
Connection lost. Reconnecting in 5s... (1/3)
```

Typing `exit` ends the session cleanly with no retry.

On the **server** side the session is kept alive after an unexpected disconnect (internet drop,
crash) for the configured **reconnect grace** (default `10m`) so a reconnect re-attaches to the
same live shell and replays what you missed. The controller sends this value per-connection, so
changing it takes effect on the next connect with no agent re-bootstrap:

```bash
fleet config set session-grace 15m             # keep dropped sessions alive 15 minutes
```

It is asked during `fleet init` (and offered to existing installs via `fleet adjust-init`).

Run a one-off command on a single server:

```bash
fleet exec web-01 uptime
fleet exec web-01 "df -h /"
fleet exec web-01 "cat /etc/os-release"
```

Run a command across all servers at once, or across a tag group:

```bash
fleet exec --all uptime
fleet exec --all "free -m"
fleet exec --group role=web "uname -r"
```

Results are shown per-server with the exit code. Non-zero exits are reported as errors.

### Structured execution

For anything a script or agent will parse or gate on, use the structured flags:

```bash
fleet exec web-01 "systemctl is-active nginx" --json     # {stdout, stderr, exit_code, duration}
fleet exec web-01 "./migrate.sh" --timeout 5m --retry 2 --backoff 2s
fleet exec web-01 "rm -rf /tmp/cache" --dry-run          # prints "would run: …" and exits
fleet exec web-01 "./deploy.sh" --propagate-exit         # exit with the remote command's code
fleet exec web-01 "./deploy.sh" --idempotency-key v3-2026-06-11   # cached result on retry
```

`--json` emits a structured result; `--timeout` aborts a hung command (reported as `timed_out`,
whichever side notices the deadline first); `--retry`/`--backoff` retry transport failures;
`--dry-run` previews; `--propagate-exit` returns the remote exit code; and
`--idempotency-key KEY` returns the cached result for `KEY` instead of re-running.
The safety flags (`--secret`, `--guard`, `--confirm`, `--require-approval`) are covered under
[Operating safely and unattended](#operating-safely-and-unattended).

Fan-out runs (`--all`, `--group EXPR`) execute on up to 16 servers at once; `--parallel N`
changes that (`--parallel 1` runs one server at a time). Output is still printed per server in
the same order as a sequential run. A fan-out exits non-zero when any server failed (blocked by
policy, unreachable, timed out, or a non-zero exit); with `--propagate-exit` the first non-zero
remote exit code, in target order, becomes the exit status. With `--json` the array lists every
target: servers where the command did not run carry `"status"` (`blocked`, `staged`, `dry-run`
or `cached`) and, when blocked, `"error"`; in `--json` mode only a policy block makes the exit
status non-zero, and informational notes go to stderr so stdout stays valid JSON. A `--group`
that matches no server is an error.

```bash
fleet exec --group role=web --parallel 32 -- "systemctl reload nginx"
fleet exec --all --json -- uptime | jq '.[] | select(.status == null) | .server'
```

## Services

Track services you care about:

```bash
fleet service add web-01 nginx.service --log /var/log/nginx/access.log --critical
fleet service list web-01
```

Control them on Linux agents:

```bash
fleet service start web-01 nginx.service
fleet service restart web-01 nginx.service
fleet service stop web-01 nginx.service
```

## Logs

Read a tracked log live:

```bash
fleet service logs web-01 nginx.service
```

Follow a log:

```bash
fleet service logs web-01 nginx.service --follow
```

Tailing reads backwards from the end of the file, so it stays fast on multi-gigabyte logs, and
line numbers are exact. Following resumes each poll from a cursor on the agent, so only newly
written bytes are read and no line is lost or repeated during bursts; a truncated or rotated
file is detected and read again from its start. (Agents older than this release are still
followed the previous way: a fresh tail each poll.)

Read the controller-cached copy:

```bash
fleet service logs web-01 nginx.service --cached
```

The aggregated cache is useful when:

- you want a quick local tail in the dashboard
- the service was already observed earlier
- you want bounded local retention instead of permanent raw log hoarding

## Metrics

Collect a live snapshot:

```bash
fleet server metrics web-01
```

The daemon also polls metrics on a schedule (`runtime.metrics_poll_interval`, default `1m`) and
feeds alert evaluation. Each cycle collects from up to 16 servers at once with a 20-second limit
per server, so a hung agent can no longer stall the poller. Every sample is kept as history
(used by the dashboard's sparklines) for 30 days; the daemon prunes older samples hourly.

## Alerts

View alerts:

```bash
fleet alerts
fleet alerts --server web-01
fleet alerts --severity critical
```

Acknowledge or suppress them:

```bash
fleet alerts ack <alert-id>
fleet alerts suppress <alert-id> --for 6h
fleet alerts unsuppress <alert-id>
```

Current alert coverage includes:

- metric thresholds
- metrics collection failures
- desktop notifications for new critical alerts
- suppression windows and cooldown reminders

## Firewall and Ports

These flows are Linux-first today.

```bash
fleet firewall status web-01
fleet firewall enable web-01
fleet firewall add web-01 "allow 443/tcp"
fleet port list web-01
fleet port open web-01 443
fleet port close web-01 443
```

On unsupported platforms, the agent returns typed unsupported-capability errors instead of pretending success.

## Tags and Groups

Label servers with `key=value` tags and target the matching set with `--group`:

```bash
fleet tag web-01 role=web env=prod      # set tags (an empty value, key=, deletes it)
fleet tag web-01                        # show one server's tags
fleet tag --list                        # all servers and their tags
```

A *group expression* is one or more `key=value` pairs joined by commas (AND): `role=web` or
`role=web,env=prod`. Many commands accept `--group EXPR` to fan out across the matching
servers — `fleet exec`, `fleet run`, `fleet health`, `fleet top`, `fleet agent update`, and
`fleet file diff` among them.

## Fleet Health and Observability

Get a per-server health view across the whole fleet:

```bash
fleet health                     # table: offline, no-swap, disk-full, reboot, clock-skew, high-load
fleet health --json              # machine-readable report
fleet health --group role=web --watch --disk 90 --load 1.5
```

A live resource table and a single-server checklist:

```bash
fleet top                        # live CPU/mem/swap/disk/load across servers (--once for one frame)
fleet top --group role=web       # only the servers matching a tag expression
fleet doctor web-01              # agent/ports/disk/swap/reboot/clock checklist (--json)
```

`top` reads swap from the agent's metrics snapshot (agents older than this release fall back to
`free -b`) and does not write an audit entry per refresh.

A machine-readable snapshot of the fleet (hostname, IPs, OS, resources, ports, services, tags),
cached under `data/inventory.json`:

```bash
fleet inventory --json
fleet inventory --refresh        # re-probe every server and rewrite the cache
```

Structured systemd control and journal access for any unit:

```bash
fleet svc status web-01 nginx.service --json
fleet svc restart web-01 nginx.service
fleet journal web-01 --unit nginx.service --since 1h --grep error
fleet journal web-01 --unit nginx.service --follow
```

`journal --follow` resumes from journalctl's cursor on every poll, so bursts are not truncated
and nothing is printed twice. On systemd 237 or newer (with PCRE2), `--grep` runs on the server
as a literal, case-insensitive match against the message text; older systems filter locally.

Capture a config baseline and detect drift later:

```bash
fleet drift capture web-01 --paths /etc/ssh/sshd_config,/etc/fstab
fleet drift web-01               # report what changed since the baseline
```

Send fleet events (offline, job-failed, drift, destructive) to Slack or a webhook:

```bash
fleet notify add slack https://hooks.slack.com/... --on offline,job-failed,drift
fleet notify list
fleet notify test --event offline
```

The `destructive` event fires after a destructive CLI command succeeds (for example
`server remove`, `file rm`, firewall changes, `secret` changes, `sync`, `key rotate` or setting
tags). The message names the command, the target server and the operator, never other
arguments. `exec`, `ssh` and `job run` do not fire it.

Targets on loopback, private or link-local addresses are refused unless added with
`--allow-internal` (for a webhook receiver on the controller's own network); the cloud
metadata address stays blocked either way. Re-adding a target replaces its events and
this setting.

## Operating Safely and Unattended

Fleet provides guardrails so a script or AI agent can make changes without a human on every
keystroke, while keeping a constrained credential from doing more than intended.

### Scoped RBAC tokens

Mint a token scoped to a set of servers (or a tag group) and a list of commands, then present
it with `--token <id>` or the `FLEET_TOKEN` environment variable:

```bash
fleet token create --name deploy --group role=web --allow exec,service --destructive
fleet token list                 # IDs are shown as a short prefix only
fleet token revoke <id>

FLEET_TOKEN=<id> fleet exec web-01 "./deploy.sh"
```

Token flags: `--servers` (named servers), `--group EXPR` (tag scope), `--allow` / `--deny`
(top-level commands), `--allow-secret <name>` (repeatable — secrets this token may inject),
`--read-only-default` (deny non-read commands unless allowed), and `--destructive` (permit
destructive operations). Enforcement is **controller-side and fails closed**: a server-scoped
token may run **only** an in-scope server command or a small set of safe local commands —
anything else (controller management like `config`/`key`/`backup`, interactive multi-server
UIs, fan-out reads, or cross-server transfers it can't fully vet) is denied, it can never mint
or modify tokens, and it can inject **only** the secrets in its `--allow-secret` list (an
unscoped admin token is unrestricted). Token IDs are stored hashed at rest.

### Named secrets

Store credentials by name (never echoed) and inject them per-command as environment variables;
the value is redacted from stdout, stderr, and the audit log:

```bash
fleet secret set deploy_key --generate 40      # or --value …, or from stdin
fleet secret list                              # names and creation times only
fleet secret rotate deploy_key --length 40
fleet secret rm deploy_key

fleet exec web-01 "./deploy.sh" --secret DEPLOY_KEY=@deploy_key
```

`VAR=@name` resolves a stored secret into `$VAR`; `VAR=literal` injects a literal. The variable
is set in the environment of the whole remote command (every part of `a; b`, pipelines and
subshells), and agents with the `exec.env` capability receive it outside the command line, so the
value never shows up in the remote process list. Older Linux/macOS agents get an
`export VAR='…';` prefix instead; older Windows agents refuse `--secret` with an error asking for
an agent update. Add reusable
redaction patterns with `fleet policy set redact-pattern '<regex>,<regex>'` (toggle the built-in
defaults with `redact-defaults on|off`).

### Audit trail

Every state-changing action is appended to a hash-chained audit log (`logs/_audit.log`), readable
with `fleet logs` and shown in the dashboard's **Ops** view. Alongside server, file, key and
config changes it records:

- `exec.run` — each remote command per server, with its exit code, `timed_out=true` or the error
  (secret values appear only as `VAR=@name`; `on_fail=true` marks `--on-fail` runs)
- `approval.stage`, `approval.approve`, `approval.reject` — the approval id and command
- `cmd-policy.set`, `policy.set` (the redaction pattern count only)
- `secret.set`, `secret.rotate`, `secret.remove` — the secret's name, never its value
- `token.create`, `token.revoke` — the token name and its short display id, never the full id
- `file.delete.failed` (and other `*.failed` file actions) for refused or failed attempts

### Dead-man's-switch (auto-rollback)

Run a risky change behind a detached, server-side timer that auto-reverts unless you confirm in
time — so a botched firewall or sshd change can't lock you out:

```bash
fleet guard web-01 "ufw default deny incoming && ufw reload" \
  --revert-after 2m \
  --revert-cmd "ufw default allow incoming && ufw reload"
fleet confirm <id>               # keep the change (cancels the revert)
fleet revert  <id>               # undo it now
```

`fleet exec ... --guard` is a lighter check: it refuses commands that could lock the controller
out of the server (downgrade to a warning with `--guard-warn`).

### Command policy and approvals

Deny or confirm-gate dangerous commands fleet-wide, and stage commands for human sign-off:

```bash
fleet cmd-policy set deny "rm -rf /,mkfs*"     # substring, or glob when * / ? present
fleet cmd-policy set confirm "reboot,shutdown*"
fleet cmd-policy show

fleet exec web-01 "reboot" --confirm           # required for a confirm-flagged command
fleet exec web-01 "./deploy.sh" --require-approval   # stage instead of running
fleet approvals list
fleet approve <id>                             # review, confirm and run it; or: fleet approvals reject <id>
```

`--require-approval` stages the command together with its exec options (`--timeout`, `--retry`,
`--backoff`, `--guard`, `--confirm`, `--on-fail`, `--idempotency-key`, and secrets as
`VAR=@name` references — a literal `--secret` value is refused, so no secret value is written to
`approvals.json`), and records who staged it; only a registered server can be staged.
`fleet approvals list` shows every staged option. `fleet approve <id>` first prints the full
request — server, command, every option, who staged it and when — and asks for confirmation
(without a terminal, pass `--yes` after reviewing it). It then runs the command once, through the
normal `fleet exec` path, so cmd-policy, guard, redaction, audit and RBAC apply again at run
time. The outcome is recorded on the approval (`executed` or `failed`, with the exit code) and
shown by `fleet approvals list`. A scoped RBAC token cannot approve.

## Playbooks

Apply a multi-step change as an idempotent, transactional playbook. For each target server every
step runs in order: if its `check` exits 0 the step is already satisfied and its `apply` is
skipped; otherwise `apply` runs. With `--on-fail rollback`, a failed step undoes the previously
applied steps in reverse order before stopping that server.

```bash
fleet run deploy.yaml --group role=web --dry-run            # print the resolved plan
fleet run deploy.yaml --group role=web --on-fail rollback   # apply transactionally
fleet run deploy.yaml web-01                                # a single named server
```

Targets resolve from a positional server, else `--group EXPR`, else the playbook's own `hosts`
expression.

## Scheduled Jobs and Background Jobs

Manage cron jobs on a server:

```bash
fleet cron add web-01 --name nightly-backup --schedule "0 3 * * *" --cmd "/usr/local/bin/backup.sh"
fleet cron list web-01
fleet cron rm web-01 --name nightly-backup
```

Run and track detached background jobs (they keep running on the server independent of your
session):

```bash
fleet job run web-01 "./long-import.sh" --name nightly-import   # optional --name label
fleet jobs                                     # list tracked jobs (ID, NAME, server, status…)
fleet job status <id>                          # detects completion + exit code
fleet job logs   <id> --follow                 # stream captured output
fleet job wait   <id> --timeout 30m            # block until it finishes
```

The optional `--name` label is shown in the `NAME` column of `fleet jobs` so a long-running
job is easy to recognize. Captured output goes to a `0600`, per-job unpredictably-named
logfile on the server (not world-readable).

**Job-log retention.** Detached-job logs are auto-deleted once they pass the configured
retention window (default `7d`) — a background pruner on the controller removes the finished
job records and their remote `/var/tmp/fleet-job-*.log` files on each server, plus an mtime
sweep that cleans up orphans. Change it any time:

```bash
fleet config set job-log-retention 30d         # keep job logs 30 days (7d/30d/12h… or 0=never)
```

It is also asked during `fleet init` (and offered to existing installs via `fleet adjust-init`).

## Port Tunnels

Forward a local (loopback-only) port to a host:port reachable from a server:

```bash
fleet tunnel web-01 5432:db.internal:5432      # localhost:5432 → db.internal:5432 via web-01
fleet tunnel web-01 8080:80                     # shorthand: target host = localhost on web-01
```

## Agents

Check agent versions and roll out updates in a health-gated canary order:

```bash
fleet agent version --all                      # report versions, flag mismatches
fleet agent update --group role=web --canary 1 # update 1, health-check, then the rest
```

`--canary N` updates a small batch of `N` servers first and only proceeds to the remainder once
every canary agent has reconnected, answers, and reports the new version (it retries for up to
90 seconds while the agent restarts). Host health problems on a canary (no swap, high load, a
full disk, a pending reboot, clock skew) are printed but do not stop the rollout; add
`--strict-health` to also require a healthy host.

## Shell Integration

Store named shell scripts and load the latest into every new terminal:

```bash
fleet automation set deploy --file ./deploy.sh # or pipe via stdin
fleet automation list
fleet shell-init --install                     # append the loader snippet to your shell rc
fleet autocomplete install                     # cached tab-completion (no per-shell fleet fork)
```

`fleet autocomplete install` writes a completion file your shell loads **once** (a `_fleet`
function in zsh's `$fpath`, a fish completions file, or a sourced bash file) rather than
re-running `fleet completion` on every new shell. Completion includes **live server names** —
`fleet exec <tab>`, `fleet ssh <tab>`, `fleet file list <tab>`, etc. suggest the servers in
your fleet (with their address and transport mode).

## Templates

List and apply templates:

```bash
fleet template list
fleet template apply web-01 web.toml
```

The current template flow can:

- register tracked services
- perform service start/stop/restart actions
- enable or disable the firewall
- open ports
- add firewall rules

## Keys

Inspect and rotate controller keys:

```bash
fleet key fingerprint
fleet key export-pub
fleet key audit
fleet key rotate
```

`fleet key rotate` supports both direct and reverse fleets. The result includes two lists:

- `rotated_servers` — servers whose authorized keys were updated
- `verified_servers` — servers where the new key was confirmed working via a live test connection before the old key was removed

If a server appears in `rotated_servers` but not `verified_servers`, the rotation was rolled back for that server. A fully successful rotation has both lists identical.

For reverse-mode servers, the rotation flow is:
1. Sends the new controller host key to the agent (agent now trusts both old and new)
2. Promotes the new key files on the controller
3. Disconnects the reverse session — agent reconnects under the new key
4. Verifies reconnect succeeded within 45 s
5. Removes the old controller host key from the agent's trusted list

## Backup and Recovery

Create a timestamped backup of the entire config directory:

```bash
fleet backup
fleet backup --output /backups/fleet-$(date +%Y%m%d).tar.gz
```

The archive includes server records, keys, audit logs, and database files. Lock files, WAL journals, and in-progress temp files are excluded automatically.

Restore from a backup:

```bash
fleet config restore /backups/fleet-20260417.tar.gz
```

After reinstalling the OS or moving to a new machine, re-attach fleet to an existing config directory:

```bash
fleet recover --from-dir /mnt/old-drive/.cenvero-fleet
```

`fleet recover` checks database connectivity, verifies the config is readable, and prints the exact `--config-dir` flag and `FLEET_CONFIG_DIR` export for your shell profile. If the config was written by a different fleet version, it tells you which version to match before proceeding. Use `--skip-version-check` only if you know what you are doing.

## Config Migrations

After upgrading fleet, you may see:

```
⚠  Your fleet config (init_version=1) is behind this version (init_version=2).
   Run 'fleet adjust-init' to review and apply configuration changes.
```

Run:

```bash
fleet adjust-init
```

This walks through every pending change interactively:

- `[✕] removed field` — shows what was removed and why, cleans up the config entry
- `[+] added field` — prompts for a value for the new option

The config is saved and stamped with the current `init_version` when done. It is safe to run at any time.

## Dashboard

Launch the TUI:

```bash
fleet dashboard
```

The dashboard is a live operations console. It refreshes in the background every 5 seconds
(`+`/`-` change the interval, `p` pauses, `r` refreshes now); the header shows how old the data
is and keeps the last good snapshot on screen if a refresh fails. It fits any terminal from
80×24 up, works with the keyboard and the mouse, and stays readable on 256- and 16-colour
terminals and with `NO_COLOR`.

Tabs:

- **Overview** — online/degraded/offline counts, alert counts by severity and state, fleet
  resource averages with p95, top CPU/memory/disk servers, open alerts, recent activity
- **Servers** — a sortable table (`o`/`O` or click a column header) with an incremental filter
  (`/`; words are ANDed, `tag=value` works); `enter` opens a detail pane with CPU/memory/disk
  history sparklines from the metrics history
- **Services**, **Logs** (a scrollable, searchable log viewer), **Alerts** (`v`/`t` filter by
  severity/state), **Ops** (the audit trail)

Actions re-run the same `fleet` binary, so RBAC tokens, cmd-policy, host-key pinning and audit
apply exactly as they do on the command line: `s` opens an SSH shell, `f` the file manager and
`L` follows a log (the dashboard suspends meanwhile); `c` reconnects a server, `R` restarts a
service, `m` collects metrics, and on the Alerts tab `a`/`z`/`u` acknowledge, suppress and
unsuppress (each asks for confirmation first). Press `?` for the full key reference.
