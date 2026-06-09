# File Manager and Transfers

Cenvero Fleet includes a secure, integrated file manager for moving files to and
from managed servers. Every transfer rides the **same authenticated,
host-key-pinned `fleet-rpc` SSH channel** the controller already uses — there is
no separate port, daemon, or unauthenticated surface.

Transfers are:

- **Chunked** — large files are split into bounded chunks (raw chunk capped at
  8 MiB; default 4 MiB) so they fit the protocol envelope after base64 framing.
- **Parallel** (direct mode) — a single SSH connection opens several `fleet-rpc`
  channels and ships chunks concurrently for high throughput.
- **Checksummed** — every chunk carries a SHA-256, and the whole file is
  SHA-256-verified on finalize, so corruption from packet loss is caught.
- **Resumable** — an interrupted transfer can be re-run and picks up where it
  left off. The controller probes the partially written remote temp file (or the
  partial local download), re-verifies the existing prefix, and only sends the
  missing chunks. The destination is committed atomically (temp file → fsync →
  `rename`).

> Reverse-mode servers transfer over their single tunnel channel: still chunked,
> checksummed, and resumable, but single-stream (no parallelism).

## CLI

```bash
# Browse
fleet file list <server> [path]

# Transfer (chunked, parallel, resumable)
fleet file upload   <server> <local> [remote] [--parallel N] [--chunk-size 4M]
fleet file download <server> <remote> [local]  [--parallel N] [--chunk-size 4M]

# Manage
fleet file mkdir <server> <path>
fleet file rm    <server> <path> [--recursive]
fleet file mv    <server> <from> <to>
```

If `[remote]` is omitted (or ends in `/`), the file lands in the server's default
remote directory under its local base name. Re-running an interrupted `upload`
or `download` with the same arguments resumes it.

## Defaults (global and per-server)

Each transfer resolves its settings from per-server overrides, then global
defaults, then built-in defaults (`parallel=4`, `chunk=4M`). Per-server defaults
are seeded from the global defaults the first time a server is bootstrapped, and
can be tuned independently afterward.

```bash
# Show global defaults, or the effective (merged) defaults for a server
fleet file defaults show
fleet file defaults show <server>

# Set global defaults
fleet file defaults set --parallel 8 --chunk-size 8M --remote-dir /srv/incoming

# Override for one server
fleet file defaults set <server> --parallel 2 --remote-dir /data
```

## Live directory sync

```bash
fleet sync <server> <local-dir> <remote-dir> [--interval 1s] [--delete] [--parallel N]
```

`fleet sync` keeps a local directory mirrored to a directory on a server. It
pushes the whole tree once, then re-scans the local directory on an interval and
uploads any new or changed files as you edit them. The sync is **live and
one-way (local → remote)** and runs until you stop it with **Ctrl-C** — when the
command exits, syncing stops.

- `--interval` — how often to re-scan for changes (default `1s`).
- `--delete` — also remove remote files that were deleted locally (off by
  default for safety).
- `--parallel` — parallel streams per file upload.

It skips `.git` metadata and does not follow symlinks. Each changed file is sent
through the same chunked, checksummed transfer engine as `fleet file upload`.

```text
$ fleet sync web-01 ./site /var/www/site
Live sync  ./site → web-01:/var/www/site
scan every 1s · press Ctrl-C to stop

✓ initial sync complete — watching for changes…
↑ index.html
↑ assets/app.css
^C
sync stopped — 2 uploaded, 0 deleted
```

## Terminal file manager (TUI)

```bash
fleet files <server>
```

A dual-pane browser: local filesystem on the left, the remote server on the
right. Navigate with the arrow keys (`→`/`Enter` to open a directory, `←` to go
up), switch panes with `Tab`, and transfer with `u` (upload the focused local
file) / `d` (download the focused remote file). With a mouse, **drag a file from
one pane and drop it on the other** to transfer it. Live progress is shown in the
Transfers panel.

## Web GUI

```bash
fleet ui            # prints http://127.0.0.1:9445/?t=<token>
fleet ui --addr 127.0.0.1:9000
```

A browser file manager served by the controller. It binds **loopback only**,
requires the per-process token printed at startup on every request, and rejects
non-loopback origins. Browse the remote server, **drag files from your desktop
onto the page to upload them** (with live progress bars), and click a file to
download it. The same secure transfer engine runs underneath.
