# Transport Modes

Cenvero Fleet supports two transport modes: direct and reverse.

The controller can manage a mixed fleet where some servers are direct and others are reverse.

## Direct Mode

Direct mode means the controller opens an SSH session to the agent.

Use direct mode when:

- the controller can reach the remote host on an SSH port
- you want on-demand connections from the controller side
- the remote server already has a stable reachable address

Typical command:

```bash
fleet server add web-01 192.0.2.10 --mode direct --port 2222
```

### Direct-mode behavior

- the controller dials the agent as an SSH client
- the agent runs an SSH server on the fleet port (default 2222)
- host keys are TOFU-pinned in the controller `known_hosts` on first connect; subsequent connects match silently
- live RPCs for services, logs, metrics, firewall, and ports run over that session
- a controller process keeps one pooled SSH connection per server and multiplexes RPC channels
  (including file-transfer streams) over it; pooled connections are probed with SSH keepalives
  every 15 seconds and retired after 3 missed replies, so a peer that vanished without closing the
  connection is noticed within about a minute instead of hanging the next call
- when a `fleet daemon` is running, one-shot CLI commands hand their direct-mode calls to it over
  the mutually authenticated loopback control socket (see below), reusing the daemon's warm connection (one round
  trip instead of a fresh handshake, roughly 550 ms → 90 ms at 50 ms latency). Every RBAC,
  cmd-policy, redaction and audit decision still happens in the CLI first; the daemon refuses and
  the CLI dials itself if the daemon's view of the server (address, port, user, key, known_hosts)
  differs, or if the daemon is older. Set `FLEET_NO_DAEMON_RELAY=1` to always dial directly.

## Reverse Mode

Reverse mode means the agent dials out to the controller and keeps a reverse session open.

Use reverse mode when:

- the agent is behind NAT or a firewall with no inbound access
- the controller does not have a stable public address reachable from outside
- you want a roaming controller laptop to keep working against outward-dialing nodes

Typical registration and startup:

```bash
fleet server add edge-01 unknown --mode reverse
fleet start        # the controller daemon, in the background (or `fleet daemon` in the foreground)
fleet-agent reverse --controller controller.example.net:9443 --server-name edge-01 --enroll-token <token>
```

> The one-time `--enroll-token` is printed by `fleet server add`; it is required only on the agent's first connect.

### Reverse-mode behavior

- the controller runs an SSH listener (default port 9443)
- the agent authenticates with its own key; the controller pins that key in `keys/agents/<name>.pub`
- the agent pins the controller host key in its own `known_hosts`
- the agent reconnects automatically on disconnect with built-in backoff (±20% jitter, so a fleet
  does not reconnect in lockstep); after a session that was healthy, such as a controller restart,
  it reconnects within about a second instead of waiting out an old backoff
- both sides send SSH keepalives, so a dead tunnel is detected within about a minute
- while disconnected the agent queues a metrics snapshot every `--offline-metrics-interval`; the
  queue is capped (10,000 snapshots / 4 MiB, thinning the oldest half when full) and is replayed
  in pages after the session registers, each page saved in one transaction and acknowledged
  separately, so a long outage replays reliably and a lost acknowledgement never duplicates history
- other `fleet` processes (the CLI, the web UI) reach reverse agents through the daemon's loopback
  control socket; the daemon queues bursts of connections instead of dropping them, and file
  chunks cross it as binary frames when both ends support it
- a reverse agent that cannot connect says why on stderr (controller unreachable, controller
  fingerprint mismatch with both fingerprints, rejected enrollment or server name) together with
  the next retry delay; identical failures are logged once every 5 minutes, and a line is logged
  when it finally connects

### Local control socket

The daemon listens on a loopback control address (`control_address`, default `127.0.0.1:9444`)
and writes a per-run secret to `data/control.token` (owner-only). The secret never crosses the
socket: each connection starts with a challenge-response in which the daemon first proves it
holds the secret (HMAC-SHA256 over fresh nonces from both sides), and only then does the caller
send its request, authenticated the same way. Something else listening on the port while the
daemon is down therefore learns nothing and cannot answer calls. The daemon removes the token
file when it stops (on Ctrl-C or `SIGTERM`).

Current daemons mark their token (`ma1-` prefix), and a caller holding a marked token never falls
back to the older protocol. A token written by an older daemon makes the CLI speak that daemon's
original protocol for reverse-mode calls and skip the direct-mode relay, so mixed versions keep
working during an upgrade.

## Per-Server Override

The controller has a global default mode, but each server can override it individually.

Examples:

```bash
fleet server add web-01 10.0.0.10 --mode direct
fleet server add edge-01 unknown --mode reverse
fleet server mode edge-01 reverse
```

Servers configured with `--mode per-server` (or the global default `per-server`) inherit the controller's `default_transport_mode` setting.

## Shell Sessions

`fleet ssh <server>` opens a persistent interactive shell through the fleet agent on a
direct-mode server. Reverse-mode servers are refused straight away (the agent has no inbound
port); use `fleet exec` for them.

Once a shell is established, the session survives network drops. If the connection is lost,
fleet reconnects automatically:

```
Connection lost. Reconnecting in 5s... (1/3)
```

Up to 3 retries are attempted with a 5-second gap. If the first connection fails, `fleet ssh`
reports why and exits without retrying. Typing `exit` ends the session cleanly — no retry loop is
triggered — and `fleet ssh` exits with the remote shell's exit status (agents older than this
release always report 0), so `echo 'make test' | fleet ssh web-01` can be used in scripts.

The host fingerprint is shown only the first time a host is pinned. Subsequent connects to the same server print nothing unless the key has changed.

## Key Rotation

`fleet key rotate` supports both direct and reverse fleets.

**Direct mode:** the controller pushes the new public key to the agent's `authorized_keys`, verifies a live test connection with the new key, then removes the old key.

**Reverse mode:** the controller sends the new host key to the agent (agent now trusts both), promotes the new key locally, disconnects the reverse session, waits for the agent to reconnect under the new key (up to 45 s), then removes the old host key from the agent's trusted list.

The rotation output includes `verified_servers` — the subset of rotated servers where the new key was confirmed working before the old key was removed.

## Security Notes

Both transport modes share the same core security posture:

- Ed25519 by default (RSA-4096 also available)
- AEAD-only SSH ciphers (`chacha20-poly1305`, `aes256-gcm`)
- host-key pinning instead of blind trust on every reconnect
- typed controller↔agent RPCs instead of arbitrary shell strings
- the local reverse-hub control socket is protected by a per-session token stored in `data/control.token`

## Operational Tradeoffs

Direct mode is often simpler when the controller has stable connectivity.

Reverse mode is often easier when:

- the agent is behind NAT
- the controller moves between networks
- you want the remote node to maintain the connection

The right choice depends on reachability, trust boundaries, and operational preference, not on a single global rule.
