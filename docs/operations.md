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

For Linux-first remote bootstrap:

```bash
fleet server bootstrap web-01 --login-user root --agent-binary ./fleet-agent
```

Reverse-mode servers should be registered before the agent dials in:

```bash
fleet server add edge-01 unknown --mode reverse
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

The daemon can also poll metrics on a schedule and feed alert evaluation.

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

The rotation path now supports:

- direct fleets
- reverse fleets
- rollback on verification or cleanup failure

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
