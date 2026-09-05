# Publishing Stable Releases to WinGet

> [!IMPORTANT]
> Read this README in full before using this guide: [`README.md`](../README.md). It defines supported platform boundaries, the controller/agent lifecycle split, and the security assumptions this procedure preserves.

This is the canonical maintainer runbook for publishing a stable Cenvero Fleet release to Microsoft's community WinGet catalog. A GitHub Release and a Microsoft catalog publication are separate events: pushing a stable tag starts Fleet's release automation, while WinGet availability begins only after a maintainer submits the generated manifests and Microsoft merges the resulting pull request.

## Policy and non-goals

- Stable releases use exact tags such as `v2.4.2`. Alpha, beta, and release-candidate tags are not submitted to WinGet.
- WinGet owns a WinGet-installed controller binary. Fleet must not replace it, create an automatic upgrade task, or run the direct PowerShell installer from the package.
- WinGet source refresh is not a background package upgrade. Operators explicitly run `winget upgrade --id Cenvero.Fleet --exact --source winget`, or opt into their own Scheduled Task or enterprise policy.
- Direct installers and Fleet-managed updates remain fail-closed on minisign. Microsoft uses the submitted `InstallerSha256` values and its own validation/scanning pipeline.
- Never replace an archive attached to a published tag. If package contents change, publish a new Fleet version and a new WinGet manifest version.
- Do not store a classic `public_repo` token or `WINGET_CREATE_GITHUB_TOKEN` in Fleet's Actions secrets. Submission uses the maintainer's existing local `gh auth` and requires interactive confirmation.

## What is automatic

Pushing a stable `v*` tag starts `.github/workflows/release.yml`. The workflow:

1. verifies that the tag commit is on `main`;
2. builds `fleet` and `fleet-agent` for the supported target matrix;
3. creates checksums and bound minisign signatures;
4. publishes the GitHub Release and updates the Homebrew formula;
5. smoke-tests the signed release matrix;
6. updates Fleet's public release manifest and Pages deployment;
7. generates the `winget-manifests-v<version>` artifact; and
8. validates, installs, executes, and uninstalls the local WinGet manifest on `windows-latest`.

The workflow intentionally does not open a Microsoft pull request. That final cross-repository mutation remains local and deliberate.

## One-time workstation setup

Required commands:

- `git`
- [GitHub CLI](https://cli.github.com/) as the GitHub account that will author the Microsoft pull request
- `jq`
- `minisign`
- `unzip`

Authenticate and configure normal Git transport without printing a token:

```bash
gh auth login
gh auth status
gh auth setup-git
```

Fork [`microsoft/winget-pkgs`](https://github.com/microsoft/winget-pkgs) once. The guarded submit helper requires an existing fork and never creates or deletes one:

```bash
gh repo fork microsoft/winget-pkgs --clone=false --default-branch-only
```

The contributing account must satisfy Microsoft's CLA when prompted. Microsoft also requires moderator approval for community submissions.

## 1. Prepare and tag the stable release

Update release notes and all user-facing documentation, then run the full preflight from a reviewed `main` commit:

```bash
make release-ready
git status --short
```

Create a signed tag when a signing key is configured:

```bash
git tag -s v2.4.2 -m "Release v2.4.2"
git push origin v2.4.2
```

If tag signing is unavailable, stop and inspect the repository's signing policy before deliberately choosing an annotated tag. Never move or reuse a published tag.

Wait for the tag-triggered `release` workflow to finish successfully. Do not submit a manifest from a failed or still-running release workflow.

## 2. Prepare a verified local submission

From the Fleet repository, run:

```bash
./scripts/prepare-winget-submission.sh v2.4.2
```

The helper is read-only with respect to GitHub. It:

- requires a stable `vMAJOR.MINOR.PATCH` tag on `origin/main`;
- requires a published, non-draft, non-prerelease GitHub Release;
- selects the successful release workflow run for that exact tag commit;
- downloads the workflow's `winget-manifests-v2.4.2` artifact;
- downloads both Windows ZIPs and their `.minisig` sidecars;
- verifies the archives with the public key committed at the release tag;
- requires the exact trusted comments for Windows x64 and ARM64;
- verifies archive members and manifest SHA-256 bindings; and
- produces exactly three canonical manifest files plus non-secret metadata.

The default output is:

```text
/tmp/cenvero-fleet-winget-v2.4.2/
├── assets/
├── manifests/c/Cenvero/Fleet/2.4.2/
│   ├── Cenvero.Fleet.yaml
│   ├── Cenvero.Fleet.locale.en-US.yaml
│   └── Cenvero.Fleet.installer.yaml
└── submission.json
```

Pass a second argument to choose another new output directory. The helper refuses to overwrite an existing path.

Review the metadata and all three manifests before continuing:

```bash
cat /tmp/cenvero-fleet-winget-v2.4.2/submission.json
find /tmp/cenvero-fleet-winget-v2.4.2/manifests -type f -print
```

## 3. Submit with explicit local confirmation

Run the guarded helper from an interactive terminal:

```bash
./scripts/submit-winget-release.sh \
  v2.4.2 \
  /tmp/cenvero-fleet-winget-v2.4.2
```

Before any remote mutation, the helper revalidates the prepared archives and manifests, checks the official catalog and open pull requests, verifies the authenticated account's existing fork, creates a temporary checkout from current upstream `master`, and stages exactly the three expected files. It prints the complete staged diff and requires this typed confirmation:

```text
submit Cenvero.Fleet 2.4.2
```

Only then does it commit, push a version-specific branch to the maintainer's fork, and open the upstream pull request. It refuses non-interactive or CI execution and fails closed if `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, or `GITHUB_ENTERPRISE_TOKEN` is set, ensuring `gh` uses the maintainer's stored local authentication. It never calls `gh auth token` or accepts `--token`.

To use a fork owned by a different authenticated account, set its owner explicitly:

```bash
FLEET_WINGET_FORK_OWNER=<github-login> \
  ./scripts/submit-winget-release.sh v2.4.2 /tmp/cenvero-fleet-winget-v2.4.2
```

## 4. Monitor Microsoft validation

The pull request is not finished merely because it is open. Confirm that all Microsoft checks pass, including:

1. pull-request validation;
2. manifest validation;
3. URL and domain validation;
4. manifest policy validation;
5. catalog-content verification;
6. installer scanning;
7. installation validation;
8. installer metadata validation;
9. final validation; and
10. the Microsoft CLA check.

Use:

```bash
gh pr checks <PR_NUMBER> --repo microsoft/winget-pkgs --watch
```

Fix deterministic findings on the same fork branch. CLA acceptance and moderator approval are external human gates; never automate legal acceptance or approval.

## 5. Verify official catalog publication

After Microsoft merges the pull request, allow time for catalog propagation, then verify from Windows:

```powershell
winget source update
winget show --id Cenvero.Fleet --exact --source winget --versions
winget upgrade --id Cenvero.Fleet --exact --source winget
```

Do not announce that a version is available through WinGet until `winget show` returns that version from the official `winget` source.

## Failure handling

- **Release workflow failed:** fix the cause; do not submit its artifact. If published binaries are wrong, issue a new patch version rather than replacing assets.
- **Prepared verification failed:** treat the release or artifact as untrusted until the mismatch is explained. Do not bypass minisign, trusted-comment, archive-member, or SHA-256 checks.
- **Duplicate version already exists:** stop. Never replace a Microsoft catalog version through this normal update flow.
- **Matching pull request already exists:** continue work on that pull request instead of opening a duplicate.
- **Microsoft scan or install validation failed:** inspect Microsoft's result and correct the release/package. Do not suppress or route around the check.
- **Only moderation is pending:** wait and report the external hold accurately.

## Stable-release completion checklist

A stable release is fully distributed only when all applicable items are complete:

- [ ] `README.md` and release documentation were read and updated.
- [ ] `make release-ready` passed.
- [ ] The stable tag points to the intended commit on `main`.
- [ ] The GitHub Release workflow passed.
- [ ] The signed asset matrix and public Fleet manifest were verified.
- [ ] `prepare-winget-submission.sh` passed without bypasses.
- [ ] The exact three-file Microsoft pull request was opened.
- [ ] Microsoft validation, CLA, and moderator review passed.
- [ ] The new version appears in the official WinGet source.
