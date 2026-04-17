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
│   ├── agents/            ← pinned public keys for reverse-mode agents
│   └── rotations/         ← archived key material from past rotations
├── servers/
├── templates/
├── logs/
│   ├── _aggregated/
│   └── _audit.log
├── alerts/
├── data/
│   ├── state.db
│   ├── metrics.db
│   ├── events.db
│   └── control.token      ← per-session secret for local reverse-hub control socket
├── backups/
└── tmp/
```

## Config File

`config.toml` stores the controller's persistent settings, including:

- product and instance metadata
- `init_version` — tracks which config migrations have been applied; used by `fleet adjust-init`
- `last_seen_version` — stamped each time a fleet command runs; used by `fleet recover` to detect version mismatches
- default transport mode
- crypto configuration
- update channel and policy
- database backend configuration
- runtime paths and controller listen addresses

Helpful commands:

```bash
fleet config show
fleet config validate
fleet config export
```

## Config Migrations

When the fleet init wizard gains or removes options across versions, `config.toml` can drift out of sync. Fleet detects this on every command via the `init_version` field and prints a hint when a migration is pending.

To apply pending migrations interactively:

```bash
fleet adjust-init
```

See [Operations Guide — Config Migrations](operations.md#config-migrations) for details.

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

When you read or follow tracked service logs, the controller stores an aggregated cached copy under:

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

## Backup and Restore

Create a full backup of the config directory:

```bash
fleet backup
fleet backup --output /path/to/fleet-backup.tar.gz
```

The archive includes server records, keys, audit logs, and database files. Lock files, WAL journals, and in-progress temp files are excluded.

Restore from a backup:

```bash
fleet config restore /path/to/fleet-backup.tar.gz
```

Use `fleet config export` and `fleet config import` when you want a JSON export/import path instead of a tarball backup.

## Recovery After Reinstall or Migration

If you reinstall the OS or move the controller to a new machine, re-attach fleet to the existing config directory with:

```bash
fleet recover --from-dir /path/to/old-config
```

`fleet recover` checks:

- database files exist (SQLite) or that connectivity works (Postgres/MySQL/MariaDB)
- the config is readable and structurally valid
- the running fleet binary matches the version that last used this config

If a version mismatch is detected, fleet tells you which version to restore before proceeding. Use `--skip-version-check` only if you know what you are doing.

## Ownership Model

The project tries to keep the ownership boundaries simple:

- you choose the config directory
- you choose the database backend
- the controller stores its own logs, alerts, and audit trail
- transport and update behavior is explicit and inspectable

That local-first model is part of the product, not an implementation accident.
