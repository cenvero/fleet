# Agentic Fleet (AI control)

Cenvero Fleet is designed to be operated by AI coding agents — Claude Code, OpenAI Codex, or anything that reads an `AGENTS.md` — not just humans. Everything is a single `fleet` binary that prints JSON and talks to your servers over an SSH transport you control, so an agent can run the whole fleet from your terminal without any new API surface or cloud dependency.

## Install the integration

```bash
fleet skill claude      # ~/.claude/skills/cenvero-fleet/SKILL.md + a /fleet slash command
fleet skill codex       # ~/.codex/prompts/fleet.md
fleet skill agents      # a portable AGENTS.md in the current directory
fleet skill list        # show targets and where they install
```

Flags: `--dir` (override the install directory), `--force` (overwrite), `--print` (print instead of writing).

The installed skill is deliberately tiny. It does not hard-code a command list (which would go stale); it tells the agent to run `fleet context` first.

## `fleet context`

```bash
fleet context           # full markdown reference: concepts, safety guidance, command tree
fleet context --json    # the same as a structured command tree
```

`fleet context` is generated **live from the installed binary** by walking the CLI command graph, so it always matches the version you have. It includes:

- what Fleet is and how an agent should use it (parse JSON, check state first, confirm destructive actions);
- the concepts (transport modes, the single authenticated channel, file transfers, storage);
- every command, subcommand, and flag, with its own help text;
- common workflows.

## `fleet ai <command>` — full help for one command

```bash
fleet ai                  # full reference for every command (no concept sections)
fleet ai file upload      # complete help for one command: usage, full description, flags
fleet ai sync --json      # the same, as a JSON node
```

`fleet ai` is the **AI-facing counterpart to `--help`**. Humans keep using
`--help` for the normal concise help; an agent runs `fleet ai <command>` to get
everything about a command — usage line, full description, and every flag — in one
markdown or JSON block. Because both `context` and `ai` render straight from the
command tree, you never have to update them by hand: give a command good help
text and it appears in both automatically.

## What the agent can do

After loading the context, the agent operates Fleet with ordinary `fleet` commands:

- **Inspect** — `fleet status`, `fleet server list/show/metrics`, `fleet service list`, `fleet logs`, `fleet file list`
- **Control any managed server** — start/stop/restart services, manage the firewall and ports, run commands, rotate keys
- **Move files** — `fleet file upload/download` (chunked, parallel, resumable)
- **Guide you** — explain state, propose next steps, and confirm before anything destructive

## Safety

- The agent runs the `fleet` CLI **on your machine, with your keys**, against only the servers you added. There is no hosted control plane.
- Every action rides the same authenticated, host-key-pinned SSH channel as the rest of the controller.
- The context tells the agent to treat `server remove`, `file rm`, `key rotate`, `update apply`, `self-uninstall`, and `config restore` as destructive and to confirm first.
- Read-only commands (`status`, `server list/show/metrics`, `service list`, `logs`, `file list`, `config show`, `context`) are safe for the agent to explore freely.
