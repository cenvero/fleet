#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

export GOCACHE="${GOCACHE:-/tmp/cenvero-go-build-cache}"
export GOMODCACHE="${GOMODCACHE:-/tmp/cenvero-go-mod-cache}"

mkdir -p dist

go build -trimpath -ldflags="-s -w -X github.com/cenvero/fleet/internal/version.Version=dev" -o dist/fleet ./cmd/fleet
go build -trimpath -ldflags="-s -w -X github.com/cenvero/fleet/internal/version.Version=dev" -o dist/fleet-agent ./cmd/fleet-agent

echo "Built dist/fleet and dist/fleet-agent"
