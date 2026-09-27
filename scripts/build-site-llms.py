#!/usr/bin/env python3
"""Regenerate the Markdown mirrors and llms-full.txt for fleet.cenvero.org.

Usage: python3 scripts/build-site-llms.py public

Run it after changing any page in public/ so the Markdown mirrors and
llms-full.txt (read by AI assistants and LLM crawlers) stay in step.

Writes:
  index.md, agentic.md, whats-new.md, docs/index.md   (clean Markdown mirrors)
  llms-full.txt                                       (everything in one file)
Stdlib only; uses site_html2md.py next to this script.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from site_html2md import convert, convert_sections, tidy  # noqa: E402

PUB = sys.argv[1]
SITE = "https://fleet.cenvero.org"
FULL = f"{SITE}/llms-full.txt"


def lead(url):
    return f"> Markdown version of <{url}>. The complete reference in one file is at <{FULL}>."


PAGES = [
    # (html, canonical url, output, H1 override)
    ("index.html", f"{SITE}/", "index.md", "Cenvero Fleet — every server you run, one binary away"),
    ("agentic.html", f"{SITE}/agentic.html", "agentic.md", "Agentic Fleet — the SSH control plane built for AI agents"),
    ("whats-new.html", f"{SITE}/whats-new.html", "whats-new.md", None),
    ("docs/index.html", f"{SITE}/docs/", "docs/index.md", None),
]


def write(rel, text):
    with open(os.path.join(PUB, rel), "w", encoding="utf-8", newline="\n") as f:
        f.write(text)


for html, url, out, title in PAGES:
    write(out, convert(os.path.join(PUB, html), url, title=title, lead=lead(url)))


HEADER = """# Cenvero Fleet — complete reference

> Cenvero Fleet is a free, open-source (AGPL-3.0-or-later), self-hosted fleet manager for Linux, macOS and Windows servers. A single controller binary (`fleet`) manages agents (`fleet-agent`) over encrypted, host-key-pinned SSH — directly, or in reverse mode where the agent dials out so servers behind NAT need no inbound port. There is no cloud account or hosted control plane.

This file collects everything published on <https://fleet.cenvero.org/> in one Markdown document for AI assistants and LLM tools: key facts, a product overview, the full documentation, the `fleet-agent` reference, the agentic-AI guide and the release notes. The short index is <https://fleet.cenvero.org/llms.txt>.

## Key facts

- **Name:** Cenvero Fleet ("Fleet" for short). Controller binary: `fleet`. Agent binary: `fleet-agent`. Made by Cenvero.
- **What it is:** a self-hosted server fleet manager — an always-connected control plane for shells, fan-out commands, file transfer, live sync, services, logs, metrics, alerts and automation, driven from a CLI, a terminal dashboard, terminal and web file managers, scripts or AI coding agents.
- **License and price:** AGPL-3.0-or-later. Free, with no paid tier, no account and no hosted service.
- **Latest stable release:** v2.4.3, released 2026-09-11. Release notes: https://fleet.cenvero.org/whats-new.html
- **Platforms:** the controller runs on Linux, macOS and Windows. Release builds exist for Linux (amd64, arm64, armv7), macOS (amd64, arm64) and Windows (amd64, arm64). Agents are installed automatically on Linux servers with systemd; macOS and Windows agents are set up manually. Service and firewall management are Linux features.
- **Architecture:** the controller runs on the machine you work from (a laptop, a bastion host or a small VM) and keeps its state in a local config directory — `~/.cenvero-fleet` on Linux — with SQLite by default, or PostgreSQL, MySQL or MariaDB. Every call rides one authenticated `fleet-rpc` SSH channel with typed RPCs rather than shell strings.
- **Transport:** direct mode (the controller connects to the agent, port 2222 by default) or reverse mode (the agent dials out to the controller daemon, port 9443 by default, enrolling with a one-time token and the controller's fingerprint). One fleet can mix both.
- **Security:** Ed25519 keys (RSA-4096 optional), trust-on-first-use host-key pinning in both directions, AEAD ciphers with pinned key-exchange, MAC and host-key algorithms, fail-closed scoped RBAC tokens, encrypted named secrets, approvals, command policy, a dead-man's switch (`fleet guard`), a hash-chained audit log, and minisign-signed releases with anti-rollback updates.
- **Interfaces:** the `fleet` CLI (most commands print JSON), `fleet dashboard` (terminal dashboard), `fleet files` (dual-pane terminal file manager) and `fleet file ui` (localhost-only web file manager).
- **AI agents:** `fleet skill claude` installs a Claude Code skill and `/fleet` slash command, `fleet skill codex` a Codex prompt and `fleet skill agents` a portable `AGENTS.md`. `fleet context` prints the complete CLI reference generated from the installed binary; `fleet ai <command>` prints the full help for one command (`--json` available).
- **Install:**
  - macOS or Linux with Homebrew: `brew tap cenvero/fleet && brew install cenvero-fleet`
  - Linux or macOS install script: `curl -fsSL https://fleet.cenvero.org/install | sh`
  - Windows, in PowerShell 5.1 or later (no administrator rights needed; installs to `%USERPROFILE%\\.local\\bin` and adds it to the user PATH): `irm https://fleet.cenvero.org/install.ps1 | iex`
  - From source (Go 1.26): `git clone https://github.com/cenvero/fleet && cd fleet && make build`
- **First steps:** `fleet init`, then `fleet server add web-01 192.0.2.10 --login-user root`, then `fleet exec web-01 uptime` or `fleet dashboard`.
- **Upgrade:** `fleet update apply` (self-managed installs) or `brew upgrade cenvero-fleet` followed by `fleet sync-agent` (Homebrew).
- **Links:** source https://github.com/cenvero/fleet · releases https://github.com/cenvero/fleet/releases · changelog https://github.com/cenvero/fleet/blob/main/CHANGELOG.md · security policy https://github.com/cenvero/fleet/security/policy (private reports to security@cenvero.org)

## Released vs. in development

The documentation in this file follows the `main` branch. The latest stable release is **v2.4.3**. These items are on `main` and ship in the next release after v2.4.3 — they are **not** in v2.4.3:

- `fleet version` (v2.4.3 prints its version with `fleet --version`).
- `fleet start` / `fleet stop` running and stopping a background daemon, and `fleet status` reporting whether it runs. On v2.4.3, run `fleet daemon` under a service manager.
- `fleet approve` showing the staged request, asking for confirmation (`--yes`, `--json`) and then running the approved command.
- `fleet exec --parallel N`, `fleet top --group` filtering, `fleet notify add --allow-internal`, `fleet agent update --strict-health`, and `fleet job wait` exiting 1 when the job failed.
- Direct-mode commands relaying through a running daemon's warm connection (`FLEET_NO_DAEMON_RELAY=1` turns it off), and the mutually authenticated daemon control socket.
- `--secret` values reaching the whole remote command through its environment, and the audit log recording remote commands, approval decisions, policy changes and token and secret changes.
- `fleet file view` and `fleet file edit`: in-place file editing on the server (exact-text replace, insert, whole content, `--expect-sha256`, `--dry-run`, `--undo`) that keeps the file's owner, mode, ACLs and SELinux label and replaces it atomically. It needs updated agents (the `file.edit` RPC). On v2.4.3, `fleet file edit <server:path>` only opens `$EDITOR` and re-uploads the file.
- The rebuilt terminal dashboard (a live operations console with sparklines, filters and actions), the terminal file manager's transfer queue, previews, bookmarks and go-to, and the redesigned web UI (light and dark themes, phone layout, command palette, previews and a read-only Fleet overview).

To see what an installed binary supports, run `fleet --version`, `fleet context` or `fleet ai <command>` — both reference commands are generated from the binary itself.
"""

AGENT_REF = """## `fleet-agent` reference

`fleet-agent` runs on each managed server. On Linux with systemd, `fleet server add … --login-user USER` (or `fleet server bootstrap`) installs it to `/opt/cenvero-fleet/fleet-agent` as `cenvero-fleet-agent.service`; elsewhere you download it from https://github.com/cenvero/fleet/releases and start it yourself.

| Command | What it does |
|---|---|
| `fleet-agent serve` | Direct mode: run the SSH transport listener that the controller connects to |
| `fleet-agent reverse` | Reverse mode: connect out to the controller daemon and serve RPCs over that session |
| `fleet-agent capabilities` | Print the detected agent capabilities |
| `fleet-agent hello` | Print the initial hello payload as JSON |

| Flag | Default | What it does |
|---|---|---|
| `--listen` | `127.0.0.1:2222` | Direct-mode listen address (use `0.0.0.0:2222` to accept a remote controller) |
| `--authorized-keys` | — | File with the controller public key(s) allowed to connect |
| `--host-key` | `~/.cenvero-fleet-agent/ssh_host_ed25519_key` | The agent's SSH host key |
| `--file-root` | unrestricted | Confine file operations to these directories (repeatable) |
| `--controller` | `127.0.0.1:9443` | Reverse mode: controller daemon address |
| `--server-name` | — | Reverse mode: the server's registered name |
| `--controller-fingerprint` | — | Reverse mode: verified `SHA256:` controller host-key fingerprint required on first enrolment |
| `--enroll-token-file` | — | Reverse mode: owner-only file holding the one-time enrolment token (removed after use); preferred over `--enroll-token` |
| `--known-hosts` | `~/.cenvero-fleet-agent/known_hosts` | Reverse mode: where the controller host key is pinned |
| `--accept-new-host-key` | off | Accept a replacement controller host key after manual verification |
| `--retry-min` / `--retry-max` | `1s` / `30s` | Reverse mode: reconnect backoff |
| `--offline-metrics-interval` | `1m` | How often metrics are queued while the controller is unreachable |
| `--metrics-queue` | `~/.cenvero-fleet-agent/reverse-metrics.jsonl` | Where queued reverse-mode metrics are kept |

```sh
# Direct mode, confined to two directories
fleet-agent serve --listen 0.0.0.0:2222 --authorized-keys ~/fleet-controller.pub --file-root /srv/incoming --file-root /var/www

# Reverse mode, enrolling with the token and fingerprint printed by `fleet server add NAME unknown --mode reverse`
fleet-agent reverse --controller controller.example.net:9443 --server-name edge-01 \\
  --controller-fingerprint SHA256:... --enroll-token-file /etc/fleet-enroll.token
```
"""

FOOTER = """## Links

- Website: https://fleet.cenvero.org/
- Documentation: https://fleet.cenvero.org/docs/ (Markdown: https://fleet.cenvero.org/docs/index.md)
- Agentic AI guide: https://fleet.cenvero.org/agentic.html (Markdown: https://fleet.cenvero.org/agentic.md)
- Release notes: https://fleet.cenvero.org/whats-new.html (Markdown: https://fleet.cenvero.org/whats-new.md)
- Source code: https://github.com/cenvero/fleet
- Releases: https://github.com/cenvero/fleet/releases
- Changelog: https://github.com/cenvero/fleet/blob/main/CHANGELOG.md
- Security policy: https://github.com/cenvero/fleet/security/policy — report vulnerabilities privately to security@cenvero.org
"""


def src(url):
    return f"Source: <{url}>"


overview = convert_sections(
    os.path.join(PUB, "index.html"), f"{SITE}/",
    [("features", "Features"),
     ("interfaces", "Interfaces"),
     ("transport", "Transport: direct and reverse modes"),
     ("security", "Security model"),
     ("faq", "Frequently asked questions")],
    shift=1)

docs = convert(os.path.join(PUB, "docs/index.html"), f"{SITE}/docs/", title="Documentation",
               shift=1, lead=src(f"{SITE}/docs/"))
agentic = convert(os.path.join(PUB, "agentic.html"), f"{SITE}/agentic.html",
                  title="Agentic AI: letting AI coding agents operate your servers", shift=1,
                  lead=src(f"{SITE}/agentic.html"))
notes = convert(os.path.join(PUB, "whats-new.html"), f"{SITE}/whats-new.html", title="Release notes",
                shift=1, lead=src(f"{SITE}/whats-new.html"))

full = "\n\n".join([
    HEADER,
    "## Overview\n\n" + src(f"{SITE}/") + "\n\n" + overview,
    docs,
    AGENT_REF,
    agentic,
    notes,
    FOOTER,
])
write("llms-full.txt", tidy(full))
print("ok")
