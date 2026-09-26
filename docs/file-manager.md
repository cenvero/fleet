# File Manager and Transfers

Cenvero Fleet includes a secure, integrated file manager for moving files to and
from managed servers. Every transfer rides the **same authenticated,
host-key-pinned `fleet-rpc` SSH channel** the controller already uses — there is
no separate port, daemon, or unauthenticated surface.

Transfers are:

- **Chunked** — files are split into chunks of 1920 KiB by default, sized to fit
  one SSH channel window together with their header (a larger configured chunk
  size is accepted and capped at that at transfer time, because anything bigger
  only stalls on the window). Chunks travel as raw binary frames when both sides
  support it.
- **Parallel** (direct mode) — a transfer opens several `fleet-rpc` channels on
  the controller's pooled SSH connection to the server (no extra connections), in
  parallel, and never more channels than it has chunks. A file that fits in one
  chunk is sent in a single request.
- **Checksummed** — every chunk carries a SHA-256 that the receiver verifies. The
  whole file is verified on finalize: current agents check a digest of the ordered
  chunk checksums instead of re-reading the file, and the reported `sha256` is
  always the real SHA-256 of the content.
- **Resumable** — an interrupted transfer can be re-run and picks up where it
  left off. The agent remembers which chunks it already verified, so only the
  missing ones are sent. The destination is committed atomically (temp file →
  fsync → `rename`).
- **Guarded** — paths containing `.` or `..` components are refused for every
  operation that creates, replaces or removes something (so `rm -r /srv/app/../..`
  can never quietly become `rm -r /`). Uploading onto an existing directory puts
  the file inside it, like `cp`.

> Reverse-mode servers transfer over their reverse tunnel: still chunked,
> checksummed, and resumable.

## CLI

```bash
# Browse / inspect
fleet file list <server> [path]
fleet file stat <server> <path>                 # size, mode, mtime, type (JSON)
fleet file cat  <server> <path>                  # stream a file to stdout (checksum-verified)
fleet file tail <server> <path> [-n 200] [--search TEXT]
fleet file diff <serverA:path> <serverB:path>    # unified line diff (exit 1 if they differ)
fleet file diff --group <expr> <path>            # diff one path across every server matching the tag expression
fleet file checksum <server> <path>              # SHA-256 of a remote file

# Edit a remote file in $EDITOR (download → edit → atomic re-upload)
fleet file edit <server:path>                    # $EDITOR, fallback vi/nano; skips upload if unchanged

# Transfer (chunked, parallel, resumable; -r for whole directories)
fleet file upload   <server> <local> [remote] [-r] [--parallel N] [--chunk-size 1920K]
fleet file download <server> <remote> [local]  [-r] [--parallel N] [--chunk-size 1920K]
fleet file download <server:remote> [local]                  # combined source form, e.g. web-01:/root/x.log ./
fleet file copy     <srcServer:path> <dstServer:path> [-r]   # server → server copy (streamed; same server: on the agent)
fleet file move     <srcServer:path> <dstServer:path> [-r]   # server → server move (copy then delete)
fleet cp            <srcServer:path> <dstServer:path> [-r]   # top-level shortcut for 'fleet file copy'

# Manage
fleet file mkdir     <server> <path>
fleet file rm        <server> <path> [--recursive]
fleet file mv        <server> <from> <to>
fleet file chmod     <server> <path> <mode>          # e.g. 0644
fleet file duplicate <server> <src> <dst>            # copy a file in place on the server

# Archive (runs the host's tar/zip on the target)
fleet file compress <server> <archive> <item>...   # zip · tar.gz · tar.bz2 · tar.xz · tar
fleet file extract  <server> <archivePath>          # into the archive's directory
```

Recursive transfers (`upload`/`download`/`copy`/`move -r`) move **several files in
parallel** (a bounded worker pool) with aggregated progress, on top of each file's
own chunk parallelism.

`fleet file copy` moves bytes **directly between two servers**: within one server
the agent copies the file itself (temp file, fsync, atomic rename), and across
servers the chunks stream through the controller without a temporary copy, so it
works for every server mode; with `-r` it copies a whole directory tree.

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

> `--file-root` bounds the file **transfer/manage** operations (list, read, write,
> mkdir, delete, rename). The archive (`compress`/`extract`), permission (`chmod`),
> and `checksum` features run via the agent's shell exec and are **not** confined by
> `--file-root` — they inherit the agent user's normal permissions. Restrict the
> agent user (or its shell-exec capability) if you need a hard boundary there.

Abandoned upload temp files (`<name>.fleet-<id>.part`) left by interrupted
transfers are reaped automatically (after 24h) when a new upload to the same
directory begins.

If `[remote]` is omitted (or ends in `/`), the file lands in the server's default
remote directory under its local base name. Re-running an interrupted `upload`
or `download` with the same arguments resumes it.

## Defaults (global and per-server)

Each transfer resolves its settings from per-server overrides, then global
defaults, then built-in defaults (`parallel=8`, `chunk=1920K`; larger chunk sizes
are capped at 1920 KiB when a transfer runs). Per-server defaults
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

Other flags: `--interval` (base re-scan rate, default `1s`) and `--parallel`
(streams per file). Changed files are copied several at a time. While nothing
changes, the re-scan interval backs off (doubling, up to 8× the base interval
and at most 5 s) and snaps back on the next change. A pull scans the whole
remote tree in one request on current agents. It skips `.git` metadata, does
not follow symlinks, and copies each file through the same chunked, checksummed
transfer engine as `fleet file`.

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
- **Every operation** — new folder (`n`), new file (`N`), rename (`r`), delete
  (`d`), copy (`c`), move (`m`), **edit with syntax highlighting** (`e`),
  **compress** (`z`) / **extract** (`x`), **permissions/chmod** (`p`), **checksum**
  (`#`), **duplicate** (`D`), properties (`i`), filter/search (`/`), sort (`o`),
  **List/Icons view** (`v`), refresh (`g`) — via a **right-click context menu**,
  the toolbar, or keys. Hidden files are off by default; `.` toggles them live.
- **Drag a file/folder between panes**: a cursor-following ghost shows what you're
  moving, the target pane glows, and on drop a **Copy here · Move here · Cancel**
  menu appears (a same-pane drag onto a folder is a rename). Directory transfers
  confirm first and copy the whole tree. Live progress shows in the transfers dock.
- **Copy/move confirmation** (`c`/`m`) shows item counts, total size, source and
  destination, and any name collisions, with a choice to overwrite, skip, or keep
  both.
- **Transfer queue** — every transfer shows a progress bar, bytes done/total,
  speed and ETA, plus overall progress. `t` focuses the queue: `x`/`X` cancel
  queued transfers, `r`/`R` retry failed ones, `C` clears finished rows. Up to
  three transfers run at once (one per server).
- **Preview pane** (`P` or `F3`) — syntax-highlighted text, a hex view for binary
  files, and metadata (owner, mode, symlink target). Previews are size-capped and
  never load a large file into memory.
- **Navigation** — go to a path with Tab completion (`:` or `Ctrl+G`), fuzzy
  jump to an item (`f` or `Ctrl+P`), back/forward history (`H`/`L`,
  `Alt+←`/`Alt+→`), bookmarks and recent folders (`'` to open, `b` to bookmark;
  saved in `<config>/tui/files-bookmarks.json`), `~` for home, and mirrored
  navigation of both panes (`=`). Each server's last folder is remembered for
  the session.
- **Selection** — `Shift+↑/↓`, range mode (`V`), `Shift`/`Ctrl`-click, `Ctrl+A`,
  and `*` to invert.
- Press `?` for the full key reference. The toolbar adapts to the terminal width
  (overflow goes into `≡ More`, `F9`), the layout works from 80×24 up, and states
  such as permission denied, "outside the agent's allowed file roots", an
  unreachable server or a missing folder are shown with how to recover. File names
  and file contents can never inject terminal escape sequences.

## Web GUI

```bash
fleet file ui            # prints http://127.0.0.1:9445/?t=<token>
fleet file ui --addr 127.0.0.1:9000
```

A premium browser file manager served by the controller. It binds **loopback
only**, requires the per-process token on every request, rejects non-loopback
origins, and keeps a strict CSP. It opens **two panes** by default (Local + first
server) and you can **Add more panes (up to 6)** — each picks **Local** (the
controller's filesystem) or any server, so local↔server and server↔server both
work. Single-click selects, double-click opens/downloads, **double-click a text
file to edit it** (syntax-highlighted editor), **right-click** gives a context
menu, and a per-pane toolbar covers every op: new folder/file, rename, delete,
copy/move, **compress / extract**, **permissions**, **checksum**, **duplicate**,
upload, download, **List/Icons view**, filter/search, sortable columns, and a
hidden toggle. **Drag between any panes** for a Copy/Move popup (directories
confirm), **drag files from your desktop** to upload, and watch live progress in
the transfers dock. The same secure transfer engine runs underneath.

- **Light and dark themes** follow the system setting, with a toggle that is
  remembered. On a phone the UI shows one pane at a time with a pane switcher and a
  bottom action bar; it also works well on tablets.
- **Keyboard first**: arrows, `Enter` to open, `Backspace` to go up, `Alt+←/→` for
  back/forward, `F6` to switch panes, `Space` and `Shift` to select, `Ctrl/Cmd+A`,
  `Delete`, `F2` to rename, `Ctrl/Cmd+C`/`X` then `Ctrl/Cmd+V` in the other pane to
  copy or move, a command palette on `Ctrl/Cmd+K`, and `?` for every shortcut.
  Right-click (or long-press, or `Shift+F10`) opens a context menu.
- **Confirm dialogs** state exactly what will be copied, replaced or deleted, and
  toasts offer **Undo** where the operation can be undone. Breadcrumbs have an
  overflow menu and an editable path; each pane keeps back/forward history, and the
  layout and last folders are remembered.
- **Transfers panel** with speed, ETA, cancel and retry, and overall progress in the
  header.
- **Previews** of text (up to 256 KiB, shown as text, never rendered as HTML) and
  common raster images; SVG and HTML files are never rendered inline.
- **Streaming downloads** start sending bytes to the browser immediately and are
  verified as they go; if verification fails the download is aborted, so the browser
  shows a failed download rather than a corrupt file.
- **Very large folders** (tens of thousands of entries) scroll smoothly.
- **Fleet overview** (the *Fleet* tab): a read-only table of every server with its
  status, mode, OS, CPU/memory/disk, last seen, tags and open alerts, with filters and
  auto-refresh; *Browse* opens that server in a file pane. When the UI is started with
  `--token`, the overview is authorized like `fleet server list`, `fleet alerts` and
  `fleet tag`.

Security: in addition to the loopback bind, per-process token and strict CSP, every
mutating request must be a same-origin `POST` (another localhost port is refused),
extra isolation headers are sent, and the *Local* source refuses paths inside the
controller's config directory (keys, tokens, databases), including through symlinks.
