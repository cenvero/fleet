# Configuration and Storage

Cenvero Fleet is designed around local, operator-owned state.

The controller stores its working data inside the chosen config directory instead of pushing it into a hosted control plane.

## Default Layout

The default config directory is:

```text
~/.cenvero-fleet
```

Typical layout:

```text
~/.cenvero-fleet/
├── config.toml
├── instance.id
├── keys/
│   ├── id_ed25519
│   ├── id_ed25519.pub
│   ├── known_hosts
│   └── rotations/
├── servers/
├── templates/
├── logs/
│   ├── _aggregated/
│   └── _audit.log
├── alerts/
├── data/
│   ├── state.db
│   ├── metrics.db
│   └── events.db
├── backups/
└── tmp/
```

## Config File

`config.toml` stores the controller’s persistent settings, including:

- product and instance metadata
- default transport mode
- crypto configuration
- update channel and policy
- database backend configuration
- runtime paths and controller listen addresses

Helpful commands:

```bash
fleet config show
fleet config validate
fleet config backup
fleet config export
```

## Database Backends

The controller supports:

- SQLite
- PostgreSQL
- MySQL
- MariaDB

SQLite is the default backend and is split into separate workload files:

- `state.db`
- `metrics.db`
- `events.db`

That avoids turning the controller into one giant all-purpose SQLite file.

## Switching Backends

You can move between backends with:

```bash
fleet database show
fleet database shift --backend postgres --dsn 'postgres://user:pass@host:5432/fleet?sslmode=require'
```

The database shift flow copies workload data first and only updates controller config after the destination backend is ready.

## Aggregated Service Logs

When you read or follow tracked service logs without a search filter, the controller stores an aggregated cached copy under:

```text
logs/_aggregated/
```

That cache is bounded by:

- maximum file size
- maximum retained rotated backups
- maximum cache age

The current runtime config keys are:

- `aggregated_log_dir`
- `aggregated_log_max_size`
- `aggregated_log_max_files`
- `aggregated_log_max_age`

## Backups and Restore

The controller can back up or restore the config directory:

```bash
fleet config backup
fleet config restore /path/to/archive.tar.gz
```

Use `fleet config export` and `fleet config import` when you want a JSON export/import path instead of a tarball backup.

## Ownership Model

The project tries to keep the ownership boundaries simple:

- you choose the config directory
- you choose the database backend
- the controller stores its own logs, alerts, and audit trail
- transport and update behavior is explicit and inspectable

That local-first model is part of the product, not an implementation accident.
