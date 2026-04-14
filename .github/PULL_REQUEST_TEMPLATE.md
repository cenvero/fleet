## Summary

- Explain what changed
- Explain the operator or maintainer impact

## Scope

- What is intentionally included
- What is intentionally not included

## Verification

- [ ] `make fmt`
- [ ] `make test`
- [ ] `make build`
- [ ] `make scale` if transport, metrics, TUI, or performance-sensitive behavior changed
- [ ] `make release-ready` if release tooling, updates, signing, or docs for release flow changed

List any additional focused commands you ran:

```text
```

## Documentation

- [ ] Docs were updated if behavior, config, or release flow changed
- [ ] No docs update was needed

If no docs update was needed, explain why:

## Release and Security Impact

- [ ] No release flow impact
- [ ] No security-sensitive impact
- [ ] This change affects transport, crypto, update, bootstrap, or signing behavior

Notes:

## DCO

- [ ] Commits are signed off with `git commit -s`
