#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

export GOCACHE="${GOCACHE:-/tmp/cenvero-go-build-cache}"
export GOMODCACHE="${GOMODCACHE:-/tmp/cenvero-go-mod-cache}"
export FLEET_RUN_SCALE_TEST="${FLEET_RUN_SCALE_TEST:-1}"
export FLEET_SCALE_AGENT_COUNT="${FLEET_SCALE_AGENT_COUNT:-100}"
export FLEET_SCALE_COLLECTION_ROUNDS="${FLEET_SCALE_COLLECTION_ROUNDS:-2}"
export FLEET_SCALE_ASSERT_ALLOC_MB="${FLEET_SCALE_ASSERT_ALLOC_MB:-100}"

cd "${ROOT_DIR}"

echo "Running Cenvero Fleet scale smoke test (${FLEET_SCALE_AGENT_COUNT} reverse agents)"
go test ./internal/core -run TestScaleSmoke100ReverseAgents -count=1 -v

echo
echo "Benchmarking dashboard snapshot path"
go test ./internal/core -run '^$' -bench BenchmarkDashboardSnapshot100Servers -benchmem -count=1

echo
echo "Scale validation completed"
