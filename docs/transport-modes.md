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

## Reverse Mode

Reverse mode means the agent dials out to the controller and keeps a reverse session open.

Use reverse mode when:

- the agent is behind NAT or a firewall with no inbound access
- the controller does not have a stable public address reachable from outside
- you want a roaming controller laptop to keep working against outward-dialing nodes

Typical registration and startup:

```bash
fleet server add edge-01 unknown --mode reverse
fleet daemon
fleet-agent reverse --controller controller.example.net:9443 --server-name edge-01
```

### Reverse-mode behavior

- the controller runs an SSH listener (default port 9443)
- the agent authenticates with its own key; the controller pins that key in `keys/agents/<name>.pub`
- the agent pins the controller host key in its own `known_hosts`
- the agent reconnects automatically on disconnect with built-in backoff
- queued metrics are replayed to the controller on reconnect

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

`fleet ssh <server>` opens a persistent interactive shell through the fleet agent, regardless of transport mode.

The session survives network drops. If the connection is lost, fleet reconnects automatically:

```
Connection lost. Reconnecting in 5s... (1/3)
```

Up to 3 retries are attempted with a 5-second gap. Typing `exit` ends the session cleanly — no retry loop is triggered.

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
