# Cenvero Fleet Documentation

This directory contains the operator and maintainer-facing documentation for Cenvero Fleet.
The docs are written to match the current repository state and focus on the real operator and maintainer workflows already implemented in the repo.

## Documentation Map

- [Getting Started](getting-started.md)
- [Transport Modes](transport-modes.md)
- [Configuration and Storage](configuration-and-storage.md)
- [Operations Guide](operations.md)
- [Releases and Updates](releases-and-updates.md)

## What These Docs Cover

The documentation currently focuses on:

- how to build and initialize the controller
- how direct and reverse transport modes behave
- where Cenvero Fleet stores its data and how backend selection works
- how operators use services, logs, alerts, templates, key rotation, and updates
- how maintainers validate release readiness before pushing a real signed tag

## Publishing

The source docs live in `docs/`. Static documentation can later be rendered into `public/docs/` for `fleet.cenvero.org`.
