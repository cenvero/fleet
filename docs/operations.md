# Operations Guide

This guide covers the main day-to-day operator workflows in Cenvero Fleet.

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

This SSHes into the server, downloads the correct `fleet-agent` binary, installs it under `/opt/cenvero-fleet/`, and starts it as a systemd service.

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

`--json` emits a structured result; `--timeout` aborts a hung command; `--retry`/`--backoff`
retry transport failures; `--dry-run` previews; `--propagate-exit` returns the remote exit
code; and `--idempotency-key KEY` returns the cached result for `KEY` instead of re-running.
The safety flags (`--secret`, `--guard`, `--confirm`, `--require-approval`) are covered under
[Operating safely and unattended](#operating-safely-and-unattended).

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

The daemon also polls metrics on a schedule and feeds alert evaluation.

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
fleet doctor web-01              # agent/ports/disk/swap/reboot/clock checklist (--json)
```

A machine-readable snapshot of the fleet (hostname, IPs, OS, resources, ports, services, tags),
cached under `data/inventory.json`:

```bash
fleet inventory --json
fleet inventory --refresh        # re-probe every server and rewrite the cache
```

Structured systemd control and journal access for any unit:

```bash
fleet svc web-01 status nginx.service --json
fleet svc web-01 restart nginx.service
fleet journal web-01 --unit nginx.service --since 1h --grep error
fleet journal web-01 --unit nginx.service --follow
```

Capture a config baseline and detect drift later:

```bash
fleet drift capture web-01 --paths /etc/ssh/sshd_config,/etc/fstab
fleet drift web-01               # report what changed since the baseline
```

Send fleet events (offline, job-failed, drift) to Slack or a webhook:

```bash
fleet notify add slack https://hooks.slack.com/... --on offline,job-failed,drift
fleet notify list
fleet notify test --event offline
```

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

`VAR=@name` resolves a stored secret into `$VAR`; `VAR=literal` injects a literal. Add reusable
redaction patterns with `fleet policy set redact-pattern '<regex>,<regex>'` (toggle the built-in
defaults with `redact-defaults on|off`).

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
fleet approve <id>                             # or: fleet approvals reject <id>
```

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

`--canary N` updates a small batch of `N` servers first and only proceeds to the remainder if
every canary comes back healthy.

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

Current tabs:

- Overview
- Servers
- Services
- Logs
- Alerts
- Ops

The dashboard supports both keyboard and mouse navigation.
