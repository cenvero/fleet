# Security Policy

Security matters more than convenience in Cenvero Fleet. The project manages remote infrastructure, key material, logs, and update flows, so security issues should be handled deliberately and privately.

## Supported Versions

Until the first real public release is cut, security fixes land on the latest development line in this repository.

Once public releases begin, the intended support posture is:

- latest stable release: supported
- latest beta or release candidate: supported for security fixes relevant to testing that line
- older unstable snapshots: best effort only

If a vulnerability requires an immediate operator action, the release manifest can be used to raise the minimum supported version for the affected channel.

## Reporting a Vulnerability

Please do **not** open a public issue for a suspected vulnerability.

Send a private report to `security@cenvero.org` with:

- a short summary of the issue
- affected versions, tags, or commits
- reproduction steps or a proof of concept
- impact assessment if you have one
- any suggested mitigation or patch direction

If you are unsure whether something is security-relevant, report it anyway.

## What to Expect After Reporting

Current response targets:

- acknowledgement within 72 hours
- triage and severity discussion as soon as practical after acknowledgement
- a private coordination loop if more detail is needed

The exact disclosure timeline will depend on impact, reproducibility, and whether a fix is ready.

## Disclosure Policy

The normal policy is:

1. receive the report privately
2. confirm and scope the issue
3. prepare a fix or mitigation
4. release the fix
5. disclose enough detail for operators to assess exposure

Please avoid public disclosure before a fix or mitigation is available unless there is a compelling safety reason to do otherwise.

## Current Security Posture

The current repository already includes several core protections:

- Ed25519 is the default controller key algorithm
- SSH transport is limited to AEAD ciphers
- host keys are TOFU-pinned and mismatches are treated as critical failures
- controller and agent RPCs use typed payloads instead of shell-string commands
- controller-owned state stays inside the configured local directory
- installers and updater flows are built around minisign verification plus checksum verification
- controller key rotation is implemented for both direct and reverse fleets
- aggregated logs are controller-owned and retained with bounded cache policies

## Maintainer-Sensitive Material

Some security procedures are intentionally not public:

- signing key custody details
- emergency rotation notes
- release-environment secrets handling
- incident runbooks that should stay local

Those belong in the local `_private/` maintainer material and should not be committed.

## Security Guidance for Operators

Operators should:

- review installer and update scripts before using them in production
- prefer explicit updates over unattended updates
- protect the controller config directory and private keys with standard OS controls
- verify release signatures before trusting a published binary
- treat reverse-mode controller reachability and DNS exposure as part of their threat model

## Out of Scope for Public Issues

The following should still go through private reporting if there is any security angle:

- suspicious update behavior
- host-key pinning bypasses
- key-rotation rollback failures
- privilege-escalation paths in bootstrap or service control
- release-signing or manifest-integrity issues

Regular support questions, documentation bugs, and non-security feature requests can use the normal issue tracker.
