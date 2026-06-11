# Agentic Fleet (AI control)

Cenvero Fleet is designed to be operated by AI coding agents — Claude Code, OpenAI Codex, or anything that reads an `AGENTS.md` — not just humans. Everything is a single `fleet` binary that prints JSON and talks to your servers over an SSH transport you control, so an agent can run the whole fleet from your terminal without any new API surface or cloud dependency.

## Install the integration

```bash
fleet skill claude      # ~/.claude/skills/cenvero-fleet/SKILL.md + a /fleet slash command
fleet skill codex       # ~/.codex/prompts/fleet.md
fleet skill agents      # a portable AGENTS.md in the current directory
fleet skill list        # show targets and where they install
```

Flags: `--dir` (override the install directory), `--print` (print instead of writing).

Re-running an install just **overwrites** the existing files (no `--force`, no error) and prints where they went and a reminder to restart Claude — so you can refresh the integration any time. After installing the Claude target, **restart Claude (or launch `claude`) and type `/fleet`**: the agent runs `fleet context`, learns the whole CLI, and operates your fleet for the rest of the session (until the context is compacted).

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

- **Inspect** — `fleet status`, `fleet health --json`, `fleet inventory --json`, `fleet server list/show/metrics`, `fleet service list`, `fleet svc status`, `fleet logs`, `fleet file list`
- **Run commands programmatically** — `fleet exec <server> <cmd> --json` returns `{stdout, stderr, exit_code, duration}`; add `--timeout`, `--retry`, `--dry-run`, `--group EXPR`
- **Control any managed server** — start/stop/restart services, manage the firewall and ports, run commands, rotate keys
- **Move files** — `fleet file upload/download` (chunked, parallel, resumable)
- **Apply multi-step changes** — `fleet run <playbook.yaml>` (idempotent check/apply, `--on-fail rollback`)
- **Guide you** — explain state, propose next steps, and confirm before anything destructive

## Operating unattended

Because the agent often runs without a human watching every step, Fleet gives it guardrails (all
documented in [Operations → Operating Safely and Unattended](operations.md#operating-safely-and-unattended)):

- **Scope the credential** — run inside a token's scope with `--token <id>` / `FLEET_TOKEN`; the
  controller denies out-of-scope or destructive calls and fails closed.
- **Never inline secrets** — `fleet secret set <name>`, then `fleet exec ... --secret VAR=@name`;
  values are redacted from all output and the audit log.
- **Guard risky changes** — `fleet guard <server> <cmd> --revert-after … --revert-cmd …` auto-reverts
  unless the agent (or you) runs `fleet confirm <id>` in time; `fleet exec --guard` refuses
  lock-yourself-out commands.
- **Stage for sign-off** — `fleet exec ... --require-approval` queues a command for `fleet approve <id>`;
  `fleet cmd-policy` deny/confirm-gates dangerous patterns.

## Safety

- The agent runs the `fleet` CLI **on your machine, with your keys**, against only the servers you added. There is no hosted control plane.
- Every action rides the same authenticated, host-key-pinned SSH channel as the rest of the controller.
- The context tells the agent to treat `server remove`, `file rm`, `key rotate`, `update apply`, `self-uninstall`, and `config restore` as destructive and to confirm first.
- Read-only commands (`status`, `health`, `inventory`, `top`, `doctor`, `drift`, `server list/show/metrics`, `service list`, `svc status`, `journal`, `logs`, `file list`, `config show`, `context`) are safe for the agent to explore freely.
