# Releases and Updates

This guide covers the current update model, local preflight checks, and the release workflow used by Cenvero Fleet.

## Update Model

The default controller posture is intentionally conservative:

- channel: `stable`
- policy: `notify-only`

That means Cenvero Fleet does not silently auto-update by default. Operators explicitly choose when to apply updates.

Current commands:

```bash
fleet update check
fleet update apply
fleet update rollback
fleet update channel stable
fleet update channel beta
fleet sync-agent                 # bring every agent up to the controller version
fleet sync-agent --server web-01 # or just one (repeatable)
```

`fleet update apply` updates the controller first and then rolls updates across managed agents. Partial agent failures are reported instead of bricking the whole rollout.

`fleet sync-agent` brings managed agents up to the controller's version. It syncs servers **in parallel** (bounded concurrency) and streams **per-server progress** as each finishes — `→ checking`, `✓ updated X → Y`, `• up to date`, `✗ error` — followed by a one-line summary, while `stdout` stays clean JSON for scripting. It runs **synchronously** (it waits for every server before returning), so there are no detached, orphaned, half-updated agents. Limit it to one or more servers with `--server <name>` (repeatable).

### Update notice

With the default `notify-only` posture you are never auto-updated, but you are told when a newer release exists. The daemon re-checks the configured channel's manifest every **10 minutes** and caches the result. Any `fleet` command then surfaces a yellow **"Update your fleet"** notice on stderr when a newer version is available, with the correct command for how you installed it:

- `brew update && brew upgrade cenvero-fleet` for a Homebrew install
- `fleet update apply` otherwise

The check is cached, so it never hammers the CDN, and it is skipped entirely for dev/unversioned builds or when the update policy is disabled.

### Auto-update policy

If you set `policy: auto-update` in `config.toml`, the controller will apply updates automatically when the daemon is running. This policy is not available for Homebrew installs — Homebrew manages the binary and `fleet update apply` cannot replace it. If you accidentally configure `auto-update` on a Homebrew install, `fleet adjust-init` will detect and fix it.

## Signature and Integrity Model

The release design uses:

- `manifest.json` for version and asset discovery
- SHA-256 checksums for integrity
- minisign signatures for authenticity
- a pinned public key in installers and updater logic

Checksums alone are not enough. If both the artifact and the manifest were tampered with together, a checksum-only model could still pass. The pinned minisign public key is what proves the release was signed by the project.

Signature verification is **fail-closed on every channel**: an update whose manifest entry carries no minisign signature is **refused**. There is no exempt channel — an empty or ad-hoc channel name does not silently disable verification. A SHA-256 checksum alone is never accepted as a substitute, because a manifest-level attacker can rewrite both the binary and its checksum together; only the pinned minisign key proves authenticity. The single, explicit escape hatch is the updater's `AllowUnsigned` opt-in (surfaced as `--allow-unsigned`) — for applying an unsigned local/ad-hoc build — and it is never the silent default.

The download path is hardened independently of the signature check. Both the manifest fetch and the artifact download are confined to an **`https`-only scheme allowlist** (`http://` is permitted only for an explicit loopback mirror; `file://`, `ftp://`, and every other scheme are rejected), the manifest and artifact bodies are read under **maximum-size bounds** so an oversized response cannot exhaust memory before verification, and the decompressed binary is capped to defuse a **decompression bomb** (a small archive that inflates to gigabytes).

## Minimum Supported Version and Anti-Rollback

The manifest tracks `min_supported` per channel. Fleet keeps a rolling window of the 10 most recent releases; the minimum supported version is always the oldest in that window.

Updates are **anti-rollback protected**: `fleet update apply` refuses to install a target version **older than the currently-running version**, or **below the channel's `min_supported`** — so a replayed or stale (but validly signed) manifest cannot downgrade the binary to a known-vulnerable release. A deliberate downgrade requires the updater's explicit `AllowDowngrade` opt-in (`--allow-downgrade`); it is never the default. (Note: this is distinct from `fleet update rollback`, which restores the last backup binary and is the supported way to revert a bad update — see below.)

## Rollback

If an update causes a problem:

```bash
fleet update rollback
```

This restores the backup binary saved during the last `fleet update apply`. The rollback state is stored in `data/update-rollback.json` and is removed after a successful rollback.

## Repo-Side Validation

Before a real tag release, the repo can already validate almost everything locally.

Run:

```bash
make release-ready
```

That command runs:

- release helper script syntax checks
- `go test ./...`
- controller and agent builds
- release-tooling smoke tests
- the 100-agent scale validation harness

## Scale Validation

For the performance and fleet-smoke path specifically:

```bash
make scale
```

That runs:

- the reverse-mode 100-agent smoke test
- the dashboard snapshot benchmark

Useful environment variables:

```bash
FLEET_SCALE_AGENT_COUNT=100
FLEET_SCALE_COLLECTION_ROUNDS=2
FLEET_SCALE_ASSERT_ALLOC_MB=100
```

## GitHub Release Workflow

The repository already includes the code-side release plumbing for:

- Goreleaser packaging
- signature sync
- manifest generation
- release asset validation
- Pages deployment workflow

Maintainers should provision the signing key, release secrets, and GitHub release environment before publishing public tags. The local maintainer material is the right place for the exact operational checklist.

## Practical Release Sequence

Once GitHub is configured, a sensible release sequence is:

```bash
make release-ready
git tag -s v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

Then verify the live release before announcing it.

## Rollback Guidance

If a release is broken but not security-critical:

- repoint `channels.stable.version` in `public/manifest.json` to the previous good version
- leave the bad release available for forensics
- ship a fixed release as soon as possible

If the broken release contains a critical security fix, roll forward with a hotfix instead of pointing operators back to a vulnerable build.
