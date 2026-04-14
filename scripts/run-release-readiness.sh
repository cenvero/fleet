#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

export GOCACHE="${GOCACHE:-/tmp/cenvero-go-build-cache}"
export GOMODCACHE="${GOMODCACHE:-/tmp/cenvero-go-mod-cache}"

cd "${ROOT_DIR}"

pass() {
  printf '  [pass] %s\n' "$1"
}

todo() {
  printf '  [todo] %s\n' "$1"
}

echo "==> Checking release helper script syntax"
bash -n scripts/test-release-tools.sh scripts/run-scale-validation.sh scripts/sync-signing-key.sh \
  scripts/update-manifest.sh scripts/validate-release-env.sh scripts/validate-release-manifest.sh \
  scripts/validate-signing-assets.sh public/install

echo
echo "==> Running full Go test suite"
go test ./...

echo
echo "==> Building controller and agent binaries"
go build ./cmd/fleet ./cmd/fleet-agent

echo
echo "==> Running release tooling smoke test"
./scripts/test-release-tools.sh

echo
echo "==> Running scale validation"
./scripts/run-scale-validation.sh

echo
echo "==> Local validation summary"
pass "release helper syntax checks passed"
pass "Go test suite passed"
pass "controller and agent builds passed"
pass "release tooling smoke test passed"
pass "scale validation passed"

echo
echo "==> Live release setup status"
if grep -R "REPLACE_WITH_MINISIGN_PUBLIC_KEY" public internal/update >/dev/null 2>&1; then
  todo "sync the real minisign public key into installer and updater assets"
else
  pass "minisign public key is synced into installer and updater assets"
fi

if [[ -z "${MINISIGN_SECRET_KEY:-}" || -z "${MINISIGN_PASSWORD:-}" || -z "${MINISIGN_PUBLIC_KEY:-}" ]]; then
  todo "load the MINISIGN_* values for a live release shell and configure them in GitHub"
else
  pass "MINISIGN_* values are loaded in this shell"
fi

if [[ -z "$(git remote -v)" ]]; then
  todo "configure the real GitHub remote before push or tag validation"
else
  pass "Git remote is configured"
fi

cat <<'EOF'

Next live release steps:
  1. Sync the real minisign public key into the repo assets if placeholders remain.
  2. Configure the MINISIGN_* values and release environment in GitHub.
  3. Configure the real GitHub remote if it is not set.
  4. Push a real version tag and verify Releases, manifest, Pages, installer, and updater end to end.
EOF
