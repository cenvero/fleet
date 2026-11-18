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

# Edit a file in place on the server (see "Editing files in place" below)
fleet file view <server> <path> [--lines 20:60] [--json]   # numbered lines + the file's sha256
fleet file edit <server> <path> --old TEXT --new TEXT [--all] [--expect-sha256 H] [--dry-run] [--json]
fleet file edit <server> <path> --insert-after N --text TEXT
fleet file edit <server> <path> --edits edits.json            # several edits, all or nothing
fleet file edit <server> <path> --content FILE --expect-sha256 H   # replace the whole file
fleet file edit <server> <path> --content FILE --create [--mode 0644]
fleet file edit <server> <path> --undo [--force] | --history
fleet file edit <server> <path> ... --in-place     # a file that cannot be replaced (see below)
fleet file edit <server:path>                    # interactive: $EDITOR, fallback vi/nano

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

# Archive (runs the host's tar/zip on the target)
fleet file compress <server> <archive> <item>...   # zip · tar.gz · tar.bz2 · tar.xz · tar
fleet file extract  <server> <archivePath>          # into the archive's directory
```

Permissions (chmod), SHA-256 checksums and duplicate are operations of the terminal and
web file managers below; from the CLI use `fleet exec`, for example
`fleet exec web-01 -- chmod 0644 /srv/www/index.html` or
`fleet exec web-01 -- sha256sum /srv/releases/site.tar.gz`.

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

## Editing files in place

`fleet file edit` changes a file **on the server**, without downloading and
re-uploading it — the agent applies the edit and saves it the way a careful
editor does. It is built to be safe to hand to an AI agent:

- **Exact edits.** `--old TEXT --new TEXT` must match the file exactly
  (whitespace, indentation and line breaks included) and exactly once, unless
  `--all` is given — an edit can never land somewhere you did not intend. When a
  match fails because of indentation, the error says so. `--insert-after N --text`
  adds lines after line N (0 = top). `--edits FILE|-` applies a JSON list of edits
  in order, all or nothing:
  `[{"old":"a","new":"b"}, {"old":"x","new":"y","all":true}, {"kind":"insert","line":12,"text":"z"}]`.
  `--content FILE|-` replaces the whole file (it needs `--expect-sha256`, `--force`,
  or `--create` for a new file).
- **Only the version you looked at.** `fleet file view` prints the file's sha256;
  pass it as `--expect-sha256` and the edit is refused with `edit_conflict` if the
  file changed in between. A write by another program while the edit is being
  applied is caught the same way. `fleet config set edit-require-hash on` makes the
  hash mandatory for every non-interactive edit.
- **Permissions kept.** The new content goes to a private temp file beside the
  original, is fsynced and read back to verify its sha256, and gets the original's
  owner, group, mode (including set-uid/set-gid/sticky), POSIX ACLs, SELinux label,
  file capabilities and other extended attributes before it replaces the original
  in one atomic rename (on Windows, `ReplaceFileW`, which keeps the ACL). If any of
  that cannot be carried over, the edit fails and the file is left as it was.
  Editing through a symlink changes its target and keeps the link. Hard-linked
  files, binary files (for text edits) and files the agent user could not write
  itself are refused.
- **Network drops.** The agent only acts on a request once it has arrived whole,
  so a connection that drops mid-edit changes nothing, and readers never see a
  half-written file. Each edit carries an id: if the reply is lost, the controller
  retries and the agent answers with the first result instead of editing twice.
- **Files that cannot be replaced.** A few files can't be swapped for a new copy:
  one bind-mounted into a container (a mount point cannot be renamed over), one in
  a folder the agent may not write, or one whose owner the agent (not running as
  root) cannot give a new file. Such an edit fails with `cannot_replace` and
  changes nothing. Repeat it with `--in-place` to write the new content into the
  file itself. That keeps the same file, so its owner, mode, ACLs and labels need no
  copying, but it is **not atomic**, so Fleet makes it as safe as possible:
  - The controller first saves the file's current version in the undo history
    (even with `edit-backups 0`, until the write succeeds).
  - It then sends the write, pinned to exactly the content it saved.
  - The agent reserves the disk space it needs (on Linux), writes, fsyncs and
    reads the result back.
  - If any step fails, the agent writes the original back and nothing has changed.
  - If even that fails (`write_incomplete`), or the connection drops mid-write and
    the file can't be read back, the error says so. `fleet file edit <server> <path>
    --undo --force` then restores the saved version.

  Hard-linked files stay refused: writing one would change it under its other
  names, which were not checked against the block list or `--file-root`. The editors
  in both file managers offer the in-place write when a save fails this way, and
  `--undo` writes an in-place edit back in place.
- **Undo.** The previous version of every edited file is kept on the controller
  (`data/edit-history`, 10 versions per file by default — `fleet config set
  edit-backups N`, 0 turns it off). `--undo` restores it, but only while the file is
  still exactly what that edit produced (`--undo --force` restores it anyway);
  `--history` lists what is kept. The kept
  versions are plain copies in the controller's owner-only config directory (and so
  in `fleet config backup` archives): if the files you edit hold secrets you do not
  want copied there, set `edit-backups 0`.
- **Readable results.** Every edit prints a unified diff, the old and new sha256,
  sizes, mode and owner (`--json` for a structured result), `--dry-run` shows the
  diff without writing, and every edit is recorded in the audit log. CRLF files keep
  their line endings. Files up to 8 MiB can be viewed and edited (`fleet config set
  edit-max-size 2M` lowers the limit).

```bash
fleet file view web-01 /etc/nginx/nginx.conf
fleet file edit web-01 /etc/nginx/nginx.conf \
    --old 'worker_connections 768;' --new 'worker_connections 2048;' --expect-sha256 <sha256>
fleet exec web-01 "nginx -t && systemctl reload nginx"
fleet file edit web-01 /etc/nginx/nginx.conf --undo    # if the config test failed
```

The editors in both file managers save the same way: they keep the file's owner
and permissions, and report a conflict instead of overwriting a file that changed
on the server while it was open. Editing needs an agent with `file.edit` support;
update older agents with `fleet agent update <server>` (until then the interactive
editors fall back to the atomic upload, which does not keep the file's owner).

The terminal file manager's editor never changes bytes you did not edit. Tabs show as
`⇥` and are saved as tabs, and a file whose line breaks are all CRLF keeps them. A file it
cannot hold exactly — control characters, a lone CR, mixed line endings, invalid UTF-8, or
more than 10,000 lines — opens read-only with the reason; edit those with `fleet file edit`
or `$EDITOR`. Unsaved edits need a second `Esc` to discard. Local saves (the *Local* pane)
are atomic, keep owner, group, mode and set-id bits, and refuse a file that changed since
it was opened, has other hard links, or that you may not write; saving unchanged text
writes nothing.

In both file managers, *New file*, rename, move and duplicate never overwrite an existing
file of that name — hidden files included, even when they are not shown — and a web upload
named like an existing folder is refused rather than landing inside it.

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
fleet console                        # the web console: Fleet, Files, Control, Terminal
fleet file ui                        # the same console on the file manager (Control/Terminal off)
fleet file ui --addr 127.0.0.1:9000
fleet file ui --control --terminal   # also run commands and open shells from the browser
fleet console --background           # keep it running after the terminal closes
fleet console open --files           # open the background console (on the file manager)
fleet console status                 # where it runs and when it stops
fleet console stop
```

`fleet console` (alias `fleet web`) starts the web UI with every view on;
`fleet file ui` starts the same console on the file manager, where the *Control* and
*Terminal* views are **off unless you add `--control` / `--terminal`**. With either on,
whoever holds the session token can act on the whole fleet with this invocation's
authority (see *Security* below).

On a terminal both show a panel instead of a bare URL: **1** opens the console in your
browser, **2** shows the access link (it carries the session token: keep it private),
**3** keeps it running in the background and returns to the shell, **4** stops it. When
a console already runs in the background, the panel has the same numbers: **2** then
shows a one-time link, **3** leaves it running and **4** stops it. In a script or a pipe
they print the link and serve until interrupted, as before.

The browser is opened with a **one-time code** (`/?c=…`, usable once, within 30
seconds), never the session token: a process's arguments are visible to every account
on the machine, and the token in a browser's command line could be read by another
local user. A link that was already used makes the page say so; if you did not open it
twice, run `fleet console stop`.

A console **in the background** is a separate `fleet console serve` process, one per
controller:

- It keeps its session token in memory. `data/console.json` (address, limits, views)
  and `data/console.key` (a launch key that only lets `fleet console open` ask for a
  one-time code) are owner-only, beside the controller's own keys.
- Choosing **3** hands the token over, so tabs you already opened keep working; it asks
  whether to keep Control and Terminal on (default no). `fleet console --background`
  keeps them only with `--control` / `--terminal`.
- It stops after **an hour without use** and **8 hours** at most — **30 minutes** and
  **1 hour** with Control or Terminal on. The page's own refreshes and streams are not
  use. Running `fleet console` or `fleet file ui` again reaches it instead of starting
  another.
- It cannot be started or opened under `--token` / `FLEET_TOKEN`: it acts with the
  controller's full authority, and keeping a token for it would leave that token in a
  long-running process.

`fleet start` is unrelated: it runs the controller daemon.

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
- The tabs are **Fleet**, **Control**, **Files** and **Terminal**, in that order; the UI
  opens on Fleet. `Alt+1` to `Alt+4` switch between them from anywhere, `/` searches
  the current page, and `?` lists every shortcut.
- **Fleet overview** (the *Fleet* tab): every server as a card (status, OS, ring gauges for
  CPU, memory and disk, tags, alerts) or, with the layout switch, as a table with
  last seen and sortable columns; filters and auto-refresh apply to both.
  *Browse* opens that server in a file pane; *Manage* opens a menu of per-server actions
  (services, unit status, journal, run a command, live metrics, health checklist,
  firewall, open ports, definition, tags, reconnect, remove). *Add server* opens the
  *Add server* wizard, and each open alert has *Ack* and *Snooze 1h*. When the UI is
  started with `--token`, the overview is authorized like `fleet server list`,
  `fleet alerts` and `fleet tag`.
- **Control** (the *Control* tab, with `--control`): Fleet's settings and tools as
  screens, laid out like System Settings. *Overview* opens with a one-line summary of the
  fleet ("All 4 servers are online."), at-a-glance tiles, quick actions and recent
  activity. A sidebar lists *Overview*, *Activity*, your pinned pages and the categories
  (Servers, Services & Logs, Run & Automate, Files, Monitoring, Network & Security,
  Updates, Controller), and search finds any page by its plain-language name or its
  command name (also from the `Ctrl/Cmd+K` palette).
  - *Servers* lists your servers straight away (status, address, mode, system, agent
    version, last seen). Each has its own page with its details, usage and one-click
    actions: services, run a command, files, terminal, live usage, health checklist,
    firewall, open ports, tags, reconnect and remove.
  - *Add server* is a wizard: the server (name, address, direct or reverse), the agent
    (install it now, the sign-in user and port, Fleet's key, a key file or a password,
    sudo, the agent port), a review, then progress and the result. If the agent install
    fails, it offers to remove the half-added server and go back to the form. A reverse
    server prints a one-time join secret, so the wizard gives you the terminal command
    for it instead.
  - A page that only reads (agent versions, health, jobs, logs…) loads its data as soon
    as it opens, as tables where it can, with *Refresh* to load it again. A page that
    changes something has a button named for what it does and shows how it went, with
    the output underneath. Servers and fixed choices are pop-up menus, on/off settings
    are switches, and the less common ones sit behind *Options* and *Advanced*.
  - The command behind a page is under *Show the command* (copyable). Runs can be
    stopped (interrupt, then kill after 5 s) and run again, and *Activity* keeps the
    newest 60 you ran, with 4 MiB of output each; data a page loads by itself stays out
    of it. Up to 8 commands run at once. Star a page to pin it to the Overview and the
    sidebar.

- **Terminal** (the *Terminal* tab, with `--terminal`): interactive shells on direct-mode servers, in tabs,
  drawn by xterm.js (vendored; WebGL when available). Open one from the tab, a server card
  or its menu, Control → *Open a shell*, or the palette. Full-screen programs (`top`,
  `vim`, `htop`) work, the window size follows the browser, a reload re-attaches to
  shells that are still running, and an ended session offers *Reconnect*. Keys typed in a
  terminal go to the shell, not to the UI's shortcuts. Each tab is `fleet ssh <server>`
  run by the controller under a pseudo-terminal, with the same token, so RBAC, the rule
  that refuses `ssh` while a command policy is configured, host-key pinning and the audit
  log (`webui.terminal`) all apply. Up to 8 at once, each watched by up to 4 browser
  tabs; a shell no tab has watched for 15 minutes (a closed tab) is ended. Needs a Linux
  or macOS controller.

### How Control runs commands

Control does not reimplement commands. Each run starts the `fleet` binary itself as a
child process with this controller's `--config-dir` and the token the UI was started
with (`--token` or `FLEET_TOKEN`), so **RBAC, command policy, redaction, confirmations
and the audit log behave exactly as in a terminal**. A scoped token's limits apply to
every run, and a denied command shows the same `denied: …` message. The UI also writes a
`webui.command` audit entry per run (the command path only; arguments can carry secrets
and the child audits its own action).

- The browser picks the command and its arguments only. It cannot set `--token`,
  `--config-dir` or environment variables (the config dir is pinned ahead of the
  command, so no argument can override it), and the arguments must resolve to exactly
  the command that was picked: a subcommand named in the arguments is refused.
- Commands that need a terminal are listed but refused, with the reason: `dashboard`,
  `files` / `filemanager`, `file ui`, `config edit`, `daemon` (use `start` /
  `stop`), `init`, `recover`, `adjust-init` and `self-uninstall`.
- Commands that read or write the controller's keys, tokens or secrets, or loosen its
  guardrails, are listed as *Terminal only* and refused: `backup`,
  `config backup|restore|import|export|show`, `database show|shift`,
  `token create|revoke`, `secret set|rotate|rm`, `key rotate`, `server enroll-token`,
  `automation set|rm`, `cmd-policy set`, `policy set` and `approve`. Reading names and
  fingerprints (`token list`, `secret list`, `key fingerprint`) still works.
- There is no terminal, so prompts fail closed: only commands that read their input from
  stdin are given any. Tick `--yes` (or `--confirm` for command-policy confirm patterns)
  to approve on purpose. Commands that change things (remove, stop, restart, update
  apply, …) also ask in the browser first. Values of password and secret flags are masked
  in the run history.
- Closing the UI (`Ctrl-C`) interrupts any commands still running.

Security: in addition to the loopback bind, per-process token and strict CSP, every
mutating request (including starting or cancelling a command) must be a same-origin
`POST` (another localhost port is refused),
extra isolation headers are sent, and the *Local* source refuses paths inside the
controller's config directory (keys, tokens, databases), including through symlinks.
With `--control` or `--terminal` on, the session token is the boundary: it can run
fleet commands and open shells with the authority of the `fleet file ui` invocation, so
start the UI with a scoped `--token` to bound it, and keep the printed URL private.
