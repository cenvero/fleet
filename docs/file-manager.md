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
# Browse / inspect
fleet file list <server> [path]
fleet file stat <server> <path>                 # size, mode, mtime, type (JSON)
fleet file cat  <server> <path>                  # stream a file to stdout (checksum-verified)
fleet file tail <server> <path> [-n 200] [--search TEXT]

# Transfer (chunked, parallel, resumable; -r for whole directories)
fleet file upload   <server> <local> [remote] [-r] [--parallel N] [--chunk-size 4M]
fleet file download <server> <remote> [local]  [-r] [--parallel N] [--chunk-size 4M]
fleet file copy     <srcServer:path> <dstServer:path> [-r]   # server → server (relayed)

# Manage
fleet file mkdir <server> <path>
fleet file rm    <server> <path> [--recursive]
fleet file mv    <server> <from> <to>
```

`fleet file copy` moves bytes **directly between two servers**, relayed through the
controller (download then upload) so it works for every server mode; with `-r`
it copies a whole directory tree.

With `-r/--recursive`, `upload` takes a local directory and a remote destination
directory and ships the whole tree; `download` pulls a remote directory into a
local one. Remote-provided names are validated so a compromised server cannot
write outside the chosen local directory.

### Confining the agent (sandbox)

By default an authenticated controller can read or write any path the agent user
can (minus `/proc`, `/sys`, `/dev`). To limit the blast radius, start the agent
with one or more allowed roots — every file operation must then stay inside them:

```bash
fleet-agent serve --file-root /srv/incoming --file-root /var/www
```

Abandoned upload temp files (`<name>.fleet-<id>.part`) left by interrupted
transfers are reaped automatically (after 24h) when a new upload to the same
directory begins.

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
fleet sync <server> <local-dir> <remote-dir> [--from local|remote] [--no-delete] [--interval 1s] [--parallel N]
```

`fleet sync` keeps a local directory and a server directory mirrored, live, until
you stop it with **Ctrl-C**.

**Writer and replica.** One side is the *writer* (the source of truth); the other
is a read-only *replica* that mirrors it. Choose the writer with `--from`:

- `--from local` (default) — the local directory is the writer; it is **pushed**
  to the server, which becomes the replica.
- `--from remote` — the server directory is the writer; it is **pulled** down and
  the local directory becomes the replica.

**Mirror semantics.** The writer is copied to the replica once, then re-scanned on
an interval:

- files that are **new or differ** overwrite the replica;
- by **default**, replica files that **don't exist on the writer are deleted**, so
  the replica becomes an exact copy;
- `--no-delete` keeps the replica's extra files (it still overwrites the ones that
  differ).

Other flags: `--interval` (re-scan rate, default `1s`) and `--parallel` (streams
per file). It skips `.git` metadata, does not follow symlinks, and copies each
file through the same chunked, checksummed transfer engine as `fleet file`.

```text
$ fleet sync web-01 ./site /var/www/site
Live sync  ./site  →  web-01:/var/www/site   (local is the writer)
mirror (replica extras are deleted) · scan every 1s · press Ctrl-C to stop

✓ initial mirror complete — watching for changes…
↑ index.html
↑ assets/app.css
✗ old-page.html
^C
sync stopped — 2 copied, 1 deleted
```

## Terminal file manager (TUI)

```bash
fleet files [source...]        # aliases: fleet filemanager · fleet fm
fleet files web-01             # single server (Local on the left, web-01 on the right)
fleet files web-01 db-01       # two servers side by side (server → server)
```

A full, desktop-application-grade **dual-pane** file manager. Each pane is a
*source* — the local filesystem (`Local`) or any managed server — so you can
browse and transfer **local↔server and server↔server**. Press `s` (or click the
header) to change a pane's source.

- **Single-click selects** (it never downloads); **double-click / Enter / →**
  opens a folder, **←** goes up, `space` multi-selects, `Tab` switches panes.
- **Every operation** — new folder (`n`), rename (`r`), delete (`d`), copy (`c`),
  move (`m`), properties (`i`), refresh (`g`) — via a **right-click context menu**,
  the toolbar, or keys. Hidden files are off by default; `.` toggles them live.
- **Drag a file/folder between panes**: a cursor-following ghost shows what you're
  moving, the target pane glows, and on drop a **Copy here · Move here · Cancel**
  menu appears (a same-pane drag onto a folder is a rename). Directory transfers
  confirm first and copy the whole tree. Live progress shows in the transfers dock.

## Web GUI

```bash
fleet file ui            # prints http://127.0.0.1:9445/?t=<token>
fleet file ui --addr 127.0.0.1:9000
```

A premium **dual-pane** browser file manager served by the controller. It binds
**loopback only**, requires the per-process token on every request, rejects
non-loopback origins, and keeps a strict CSP. Each pane picks any server (two
servers = **server↔server**). Single-click selects, double-click opens/downloads,
**right-click** gives a context menu, and a per-pane toolbar covers every op
(new folder, rename, delete, copy/move, upload, download, hidden toggle). **Drag
between panes** for a Copy/Move popup (directories confirm), **drag files from
your desktop** onto a pane to upload, and watch live progress in the transfers
dock. The same secure transfer engine runs underneath.
