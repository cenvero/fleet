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
```

`fleet update apply` updates the controller first and then rolls updates across managed agents. Partial agent failures are reported instead of bricking the whole rollout.

## Signature and Integrity Model

The release design uses:

- `manifest.json` for version and asset discovery
- SHA-256 checksums for integrity
- minisign signatures for authenticity
- a pinned public key in installers and updater logic

Checksums alone are not enough. If both the artifact and the manifest were tampered with together, a checksum-only model could still pass. The pinned minisign public key is what proves the release was signed by the project.

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
