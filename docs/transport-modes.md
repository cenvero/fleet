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
fleet server add web-01 192.0.2.10 --mode direct --port 22
```

### Direct-mode behavior

- the controller dials the agent as an SSH client
- the agent runs an SSH server
- host keys are TOFU-pinned in the controller `known_hosts`
- live RPCs for services, logs, metrics, firewall, and ports run over that session

## Reverse Mode

Reverse mode means the agent dials out to the controller and keeps a reverse session open.

Use reverse mode when:

- the controller does not have a stable public address
- the agent can dial outward but cannot be reached inbound
- you want a roaming controller laptop to keep working against outward-dialing nodes

Typical registration and startup:

```bash
fleet server add edge-01 unknown --mode reverse
fleet daemon
fleet-agent reverse --mode reverse --controller controller.example.net:9443 --server-name edge-01
```

### Reverse-mode behavior

- the controller runs an SSH listener
- the agent authenticates with its own key and the controller pins that key
- the agent pins the controller host key in its own `known_hosts`
- reconnect backoff is built in
- queued metrics can be replayed after reconnect

## Per-Server Override

The controller has a default mode, but each server can override it.

Examples:

```bash
fleet server add web-01 10.0.0.10 --mode direct
fleet server add edge-01 unknown --mode reverse
fleet server mode edge-01 reverse
```

## Security Notes

Both transport modes currently share the same core security posture:

- Ed25519 by default
- AEAD-only SSH ciphers
- host-key pinning instead of blind trust on every reconnect
- typed controller↔agent RPCs instead of arbitrary shell strings

Key rotation now supports both direct and reverse fleets. For reverse fleets, the controller updates the agent’s pinned controller host key before promoting the new local key and then verifies reconnect under the new key.

## Operational Tradeoffs

Direct mode is often simpler when the controller has stable connectivity.

Reverse mode is often easier when:

- the agent is behind NAT
- the controller moves between networks
- you want the remote node to maintain the connection

The right choice depends on reachability, trust boundaries, and operational preference, not on a single global rule.
