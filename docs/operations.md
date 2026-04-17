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

Run a one-off command on a single server:

```bash
fleet exec web-01 uptime
fleet exec web-01 "df -h /"
fleet exec web-01 "cat /etc/os-release"
```

Run a command across all servers at once:

```bash
fleet exec --all uptime
fleet exec --all "free -m"
```

Results are shown per-server with the exit code. Non-zero exits are reported as errors.

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
