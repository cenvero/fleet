# Agentic Fleet — the SSH control plane built for AI agents

> Markdown version of <https://fleet.cenvero.org/agentic.html>. The complete reference in one file is at <https://fleet.cenvero.org/llms-full.txt>.

Cenvero Fleet, the open-source, self-hosted server fleet manager, turns a fleet of servers into something an AI coding agent — Claude Code, Codex — can actually understand and operate: a self-describing CLI, structured JSON and guard rails, all over one authenticated SSH channel. Your keys, your machine, no cloud.

[Connect your agent](https://fleet.cenvero.org/agentic.html#get-started) · [How it works](https://fleet.cenvero.org/agentic.html#how)

- Claude Code & Codex
- JSON everywhere
- Fail-closed tokens

*Example — Terminal: an AI agent using Fleet to roll out a release to the web tier behind a dead-man's switch*

```console
› /fleet ship ./dist to the web tier, and roll
  back on its own if health checks fail

● fleet context          60+ commands learned
● fleet file upload web-01 ./dist/site.tar.gz
  uploaded (48.2 MiB, resumable, sha256 ok)
● fleet guard web-01 --revert-after 2m …
  armed — auto-revert in 2m unless confirmed
● fleet exec web-01 --json -- curl -fsS localhost/healthz
  exit_code 0 · 38ms
● fleet confirm g-7f3a
  confirmed — change kept

web-01 is serving the new release. Rolling to
web-02 next with the same guard.
```

## Raw SSH wasn't built for an AI.

Hand an agent a bare shell and it improvises: it guesses flags, scrapes human-formatted output and has no idea which commands are dangerous. Playbooks are static; hosted agent platforms want your servers inside their control plane.

### Not discoverable

An agent can't enumerate what a server — or a tool — can do. It guesses, and guesses break.

### Unstructured output

Human-formatted text has to be scraped. One format change and the agent is lost.

### No guard rails

`rm -rf` looks like any other command. Nothing stops a confused agent at 3 a.m.

## A CLI that explains itself to the agent.

Fleet is self-describing by design. The agent learns the whole tool from the binary it is about to use, then operates with structured, predictable commands.

### `fleet context`

The complete, always-current reference — every command, concept and safety rule — generated live from the installed binary. The agent reads it once per session.

### `fleet ai <command>`

Full machine-readable help for any single command, as markdown or `--json` — the AI-facing counterpart to `--help`, always matching your version.

### Structured JSON

Most commands print JSON, and `fleet exec --json` returns stdout, stderr, exit code, duration and timeout status per server — so the agent branches on real results.

### Rails that don't trust the agent

Scoped tokens, approvals, command policy and a dead-man's switch are enforced by the controller — they hold even if the agent misbehaves.

## From zero to agent-operated in one command.

1. **Install the skill**

   `fleet skill claude` installs a Claude Code skill and a `/fleet` slash command; `fleet skill codex` adds a Codex prompt; `fleet skill agents` writes a portable `AGENTS.md`.

2. **Type `/fleet`**

   The agent runs `fleet context`, learns the whole CLI and becomes your fleet operator for the rest of the session.

3. **Ask in plain language**

   "Restart nginx on web-01", "tail the API logs", "sync ./site to the server" — the agent maps it to the right commands, with your keys, on your machine.

## A whole deployment, without leaving the tool.

Because every step is a Fleet command, an agent can carry a release from build artefact to verified traffic — and each step returns something it can check.

- **Ship** the build with chunked, parallel, resumable, checksummed uploads.
- **Run slow steps as jobs** the agent can poll, instead of holding a connection open.
- **Switch traffic behind a guard** that reverts on its own unless the agent confirms.
- **Verify** with unit status, the journal and health checks — then move to the next server.

```sh
# Ship and unpack the release
fleet file upload web-01 ./dist/site.tar.gz /srv/releases/
fleet file extract web-01 /srv/releases/site.tar.gz

# Slow step as a detached job
fleet job run web-01 "./migrate.sh" --name migrate
fleet job wait <job-id>

# Switch traffic behind a dead-man's switch
fleet guard web-01 --revert-after 2m \
  --revert-cmd "ln -sfn /srv/previous /srv/current" \
  -- "ln -sfn /srv/releases/site /srv/current"
fleet exec web-01 --json -- curl -fsS http://localhost/healthz
fleet confirm <guard-id>

# Verify
fleet svc status web-01 nginx.service --json
fleet journal web-01 --unit nginx --since 10m --grep error
```

## Safety that holds even when the agent doesn't.

Every rail is enforced by the controller before anything reaches a server — none of them rely on the model following instructions.

### Scoped, fail-closed tokens

`fleet token create` limits an agent to named servers or a tag group, an allow/deny list of commands and specific secrets. Anything else is denied; a scoped token can never mint tokens or approve its own requests.

### Secrets, never inlined

Store credentials with `fleet secret set` and inject them with `--secret VAR=@name`. Values reach the whole remote command through its environment and are redacted from output and the audit log.

### Approvals

`--require-approval` stages a command with its options. A human reviews it with `fleet approve`, which then runs it and records the outcome.

### Dead-man's switch

`fleet guard` arms a detached, server-side timer that reverts a risky change unless it is confirmed — so an agent that locks itself out recovers on its own.

### Dry runs, policy & idempotency

`--dry-run` previews, `fleet cmd-policy` denies or demands confirmation for patterns, and `--idempotency-key` makes a retried command return the cached result.

### A tamper-evident trail

Every command, transfer, approval and token or secret change lands in a hash-chained audit log — with secret values shown only as `VAR=@name`.

## Traditional SSH tooling vs. Cenvero Fleet

What an AI agent gets from each approach.

| Capability | Raw SSH | Ansible | Hosted agent platform | Cenvero Fleet |
|---|---|---|---|---|
| Discoverable, version-matched command surface | No | No | Partial | Yes — `fleet context` / `fleet ai` |
| Structured JSON results | No | Partial | Partial | Yes |
| One-command agent setup | No | No | SDK | Yes — `fleet skill` |
| Controller-enforced guard rails | No | No | Partial | Yes — tokens, approvals, guard |
| Host-key pinning | Manual | Manual | Yes | Yes — automatic |
| Resumable file transfer & live sync | scp / rsync | Modules | Varies | Yes |
| Works behind NAT without inbound ports | No | No | Yes | Yes — reverse mode |
| Self-hosted, no cloud account | Yes | Yes | No | Yes |

## Agentic SSH, answered.

### Can an AI agent like Claude control my servers over SSH?

Yes. `fleet skill claude` installs a `/fleet` slash command for Claude Code (`fleet skill codex` for Codex). Typing it makes the agent run `fleet context` and operate your servers over an authenticated, host-key-pinned SSH channel with your keys, on your machine. There is no cloud control plane.

### Why is Fleet better than raw SSH for AI agents?

Raw SSH gives an agent a free-form shell with no structure, no discoverability and no guard rails. Fleet's CLI describes itself, returns JSON, and puts controller-side limits around risky operations, so the agent learns the tool once and operates reliably.

### Can an agent run a whole deployment end to end?

Yes — upload with `fleet file upload`, unpack with `fleet file extract`, run slow steps with `fleet job run` and `fleet job wait`, switch traffic behind `fleet guard`, and verify with `fleet svc status`, `fleet journal` and `fleet exec --json` health checks. See the [example above](https://fleet.cenvero.org/agentic.html#deploy).

### Can an agent safely edit config files on a server?

Yes (next release). The agent reads the file with `fleet file view`, which shows numbered lines and the file's sha256, and changes it in place with `fleet file edit --old … --new … --expect-sha256 …`. Nothing is downloaded or re-uploaded. The text must match exactly once, and the edit only applies to the version the agent read. The file keeps its owner, mode, ACLs and SELinux label and is replaced atomically, so a dropped connection never leaves half a file. Each edit prints a diff, is audited, and can be reverted with `--undo`. See [Edit files in place](https://fleet.cenvero.org/docs/#file-edit).

### Can one agent manage many servers at once?

`fleet exec --all` fans out across the fleet and `fleet exec --group role=web` targets a tag expression, returning per-server JSON with exit codes. `fleet tag` defines the groups; `fleet health`, `fleet top` and `fleet inventory --json` give fleet-wide state.

### What stops an agent from breaking production unattended?

Rails that don't depend on the agent behaving: fail-closed scoped tokens, `--dry-run`, `--require-approval` with `fleet approve`, `--idempotency-key`, `fleet cmd-policy`, and `fleet guard`'s automatic revert. Everything is recorded in a hash-chained audit log.

### Does it need a cloud account?

No. Fleet is fully self-hosted and open source — one controller binary on your machine, SSH to your servers, no SaaS and no telemetry in the core runtime.

## Let an AI agent run your fleet — today.

Install Fleet, initialise the controller, then teach your agent the whole CLI with one command.

```sh
brew tap cenvero/fleet && brew install cenvero-fleet
fleet init
fleet skill claude    # then type /fleet in Claude Code
```

[All install options](https://fleet.cenvero.org/#install) · [Agent docs](https://fleet.cenvero.org/docs/#agentic)
