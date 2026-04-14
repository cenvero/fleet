# Contributing to Cenvero Fleet

Thanks for helping build Cenvero Fleet.

This project aims to feel like a serious Unix tool: operator-owned, predictable, secure by default, and straightforward to inspect. Contributions should move the project in that direction.

## Project Principles

When you contribute, keep these rules in mind:

- Use **Cenvero Fleet** in prose, documentation, and UI copy
- Use `fleet` for the controller binary and `fleet-agent` for the agent binary
- Preserve the operator-owned model: no cloud dependency in the core runtime
- Prefer explicit, reviewable behavior over clever hidden automation
- Treat transport, crypto, update, and storage changes as security-sensitive work
- Keep Linux-first operational features honest; do not pretend unsupported platforms are supported

## Development Environment

The repository currently targets **Go 1.26**.

Common local commands:

```bash
make fmt
make test
make build
make scale
make release-ready
```

What these do:

- `make fmt` formats Go sources
- `make test` runs the full Go test suite
- `make build` builds `fleet` and `fleet-agent`
- `make scale` runs the opt-in reverse-mode 100-agent smoke test and dashboard benchmark
- `make release-ready` runs the repo-side release preflight checks

The Makefile uses writable Go caches under `/tmp` by default so the commands work cleanly in local and sandboxed environments.

## Repository Structure

Important top-level areas:

- `cmd/`: controller and agent entrypoints
- `internal/core/`: controller runtime, config, transport orchestration, updates
- `internal/agent/`: agent runtime and RPC handlers
- `internal/transport/`: SSH transport logic and host-key pinning
- `internal/tui/`: Bubble Tea dashboard
- `internal/store/`: controller state storage
- `public/`: GitHub Pages landing page, installers, manifest, and signing assets
- `scripts/`: release and validation tooling
- `docs/`: operator-facing documentation

## Contribution Workflow

Recommended flow:

1. Create a focused branch.
2. Make the smallest change that fully solves the problem.
3. Run the relevant verification locally.
4. Update docs when user-facing behavior or maintainer workflow changes.
5. Open a pull request with a clear summary and sign-off.

Good changes tend to have:

- clear scope
- obvious operator impact
- tests for behavior changes
- docs updates when commands, config, or workflows change

## Coding Expectations

When editing Go code:

- Add SPDX headers to new source files
- Prefer small functions and explicit data flow over hidden magic
- Keep transport, crypto, database, and update changes readable and auditable
- Avoid shell-string RPC behavior; use typed payloads and structured errors
- Use parameterized or ORM-managed database access rather than string-built SQL

When editing the UX:

- Preserve the Cenvero Fleet naming rules
- Keep the TUI fast, readable, and operator-friendly
- Do not introduce empty marketing language or fake support claims

## Documentation Expectations

If your change affects how operators or maintainers use the project, update the relevant docs in the same change.

Examples:

- CLI behavior changes: update `README.md` and the relevant page in `docs/`
- Release tooling changes: update `README.md`, `docs/releases-and-updates.md`, and the local maintainer guide
- Security-sensitive behavior changes: update `SECURITY.md` and the relevant operational docs

## Testing Expectations

At minimum, run the narrowest tests that prove your change.

Typical expectations:

- Small runtime or store change: targeted package tests
- CLI or cross-cutting change: `make test`
- Release tooling or performance change: `make release-ready`

For bigger changes, include the exact commands you ran in the pull request.

## Commit Sign-off

This project uses the Developer Certificate of Origin (DCO), not a CLA.

Sign commits with:

```bash
git commit -s
```

By doing so, you certify the contribution under the terms of the DCO:

<https://developercertificate.org/>

## Pull Requests

Pull requests should explain:

- what changed
- why it changed
- how it was verified
- whether docs or release behavior changed

Use the PR template and keep the description concrete.

## Security Issues

Do **not** file public issues for suspected vulnerabilities.

Follow [SECURITY.md](SECURITY.md) instead and report privately.

## Good First Contributions

Good starter contributions usually include:

- documentation improvements
- clearer command output
- tighter tests around already-implemented behavior
- small TUI polish that does not change architecture
- release-tooling validation or docs improvements

## Naming and Branding

Please keep these naming rules consistent:

- prose: **Cenvero Fleet**
- controller binary: `fleet`
- agent binary: `fleet-agent`
- domain: `fleet.cenvero.org`

Avoid introducing “Fleet” by itself in docs or UI copy where the brand context matters.
