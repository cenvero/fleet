# Getting Started

This guide walks through the shortest path to a working Cenvero Fleet controller and agent.

## Prerequisites

- Go 1.26 to build from source
- an SSH-reachable test machine for direct mode, or a machine that can dial back to the controller for reverse mode
- a terminal capable of running Bubble Tea for the dashboard

## Install the Controller

For the public installer entrypoint:

```bash
curl -fsSL https://fleet.cenvero.org/install | sh
```

That command resolves to the hosted installer for the detected platform. On Linux and macOS it runs the POSIX installer. From a Windows-compatible shell such as Git Bash or WSL, it hands off to the PowerShell installer.

## Build the Binaries

```bash
make build
```

That builds:

- `fleet`
- `fleet-agent`

You can also build them directly:

```bash
go build ./cmd/fleet
go build ./cmd/fleet-agent
```

## Initialize the Controller

Run:

```bash
fleet init
```

The interactive flow creates:

- the config directory
- controller keys
- SQLite workload databases or another selected backend config
- audit and alert directories
- the initial `config.toml`

You can inspect the current state with:

```bash
fleet status
fleet config show
```

### After upgrading fleet

If you upgrade the binary and see a hint like `⚠ run 'fleet adjust-init'`, run:

```bash
fleet adjust-init
```

This walks through any config fields added or removed since you last ran `fleet init`, and updates `config.toml` interactively. It is safe to run at any time.

## Direct Mode Walkthrough

Direct mode is for the case where the controller can reach the agent over SSH.

Start a local or reachable test agent:

```bash
./fleet-agent serve --authorized-keys ~/.cenvero-fleet/keys/id_ed25519.pub
```

Add it to the controller:

```bash
./fleet server add demo 127.0.0.1 --mode direct --port 2222
```

Then perform the first live connection:

```bash
./fleet server reconnect demo
```

Now you can try:

```bash
./fleet server show demo
./fleet service list demo
./fleet server metrics demo
```

## Reverse Mode Walkthrough

Reverse mode is for the case where the agent can dial out but the controller is not directly reachable from the outside.

Start the controller daemon:

```bash
./fleet daemon
```

Register the server first:

```bash
./fleet server add edge-01 unknown --mode reverse
```

Then start the reverse agent:

```bash
./fleet-agent reverse --controller 127.0.0.1:9443 --server-name edge-01
```

Once connected, the reverse session is visible through the same fleet workflows:

```bash
./fleet server reconnect edge-01
./fleet server metrics edge-01
./fleet alerts --server edge-01
```

## Track a Service and Read Logs

Add a tracked service:

```bash
./fleet service add demo nginx.service --log /var/log/nginx/access.log --critical
```

Read it live:

```bash
./fleet service logs demo nginx.service
```

Follow it:

```bash
./fleet service logs demo nginx.service --follow
```

Read the controller-cached copy later:

```bash
./fleet service logs demo nginx.service --cached
```

## Launch the Dashboard

```bash
./fleet dashboard
```

Current dashboard tabs include:

- Overview
- Servers
- Services
- Logs
- Alerts
- Ops

Keyboard and mouse navigation are both supported.

## Shell Access

Open an interactive shell on any server through the fleet agent:

```bash
./fleet ssh demo
```

The shell session stays alive across network drops. If the connection is lost, fleet prints:

```
Connection lost. Reconnecting in 5s... (1/3)
```

and retries automatically up to 3 times. Typing `exit` ends the session cleanly with no retry.

Run a one-off command or run it across all servers:

```bash
./fleet exec demo uptime
./fleet exec --all uptime
```

## Backup and Recovery

Create a backup before a major change or migration:

```bash
fleet backup
fleet backup --output /path/to/fleet-backup.tar.gz
```

If you reinstall the OS or move the controller to a new machine and still have the old config directory, re-attach to it with:

```bash
fleet recover --from-dir /path/to/old-config
```

## Next Recommended Commands

Once the basic path works, the next useful commands are:

```bash
./fleet template list
./fleet key fingerprint
./fleet key rotate
./fleet update check
./fleet database show
```

For local release hardening, use:

```bash
make scale
make release-ready
```
