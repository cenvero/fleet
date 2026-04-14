#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT INT TERM

mkdir -p "${TMP_DIR}/public" "${TMP_DIR}/internal/update" "${TMP_DIR}/dist"
cp "${ROOT_DIR}/public/install" "${TMP_DIR}/public/install"
cp "${ROOT_DIR}/public/install.sh" "${TMP_DIR}/public/install.sh"
cp "${ROOT_DIR}/public/install.ps1" "${TMP_DIR}/public/install.ps1"

cat > "${TMP_DIR}/public/manifest.json" <<'EOF'
{
  "generated_at": "2026-04-13T00:00:00Z",
  "channels": {
    "stable": {
      "version": "v0.1.0-alpha.1",
      "release_date": "2026-04-13",
      "min_supported": "v0.1.0-alpha.1",
      "release_notes_url": "https://github.com/cenvero/fleet/releases/tag/v0.1.0-alpha.1"
    },
    "beta": {
      "version": "v0.1.0-alpha.1",
      "release_date": "2026-04-13",
      "min_supported": "v0.1.0-alpha.1",
      "release_notes_url": "https://github.com/cenvero/fleet/releases/tag/v0.1.0-alpha.1"
    }
  },
  "binaries": {
    "v0.1.0-alpha.1": {}
  },
  "agent_binaries": {
    "v0.1.0-alpha.1": {}
  }
}
EOF

cat > "${TMP_DIR}/public/signing.pub" <<'EOF'
untrusted comment: placeholder
REPLACE_WITH_MINISIGN_PUBLIC_KEY
EOF

cat > "${TMP_DIR}/internal/update/signing.pub" <<'EOF'
untrusted comment: placeholder
REPLACE_WITH_MINISIGN_PUBLIC_KEY
EOF

printf 'fleet-linux-amd64' > "${TMP_DIR}/dist/fleet_v1.2.3_linux_amd64.tar.gz"
printf 'fleet-linux-amd64-sig' > "${TMP_DIR}/dist/fleet_v1.2.3_linux_amd64.tar.gz.minisig"
printf 'fleet-linux-armv7' > "${TMP_DIR}/dist/fleet_v1.2.3_linux_armv7.tar.gz"
printf 'agent-linux-amd64' > "${TMP_DIR}/dist/fleet-agent_v1.2.3_linux_amd64.tar.gz"
printf 'agent-linux-amd64-sig' > "${TMP_DIR}/dist/fleet-agent_v1.2.3_linux_amd64.tar.gz.minisig"

cat > "${TMP_DIR}/dist/artifacts.json" <<'EOF'
[
  {
    "name": "fleet_v1.2.3_linux_amd64.tar.gz",
    "path": "dist/fleet_v1.2.3_linux_amd64.tar.gz",
    "goos": "linux",
    "goarch": "amd64",
    "type": "Archive",
    "extra": {
      "Binary": "fleet",
      "Checksum": "sha256:1111111111111111111111111111111111111111111111111111111111111111"
    }
  },
  {
    "name": "fleet_v1.2.3_linux_armv7.tar.gz",
    "path": "dist/fleet_v1.2.3_linux_armv7.tar.gz",
    "goos": "linux",
    "goarch": "arm",
    "goarm": "7",
    "type": "Archive",
    "extra": {
      "Binary": "fleet",
      "Checksum": "sha256:2222222222222222222222222222222222222222222222222222222222222222"
    }
  },
  {
    "name": "fleet-agent_v1.2.3_linux_amd64.tar.gz",
    "path": "dist/fleet-agent_v1.2.3_linux_amd64.tar.gz",
    "goos": "linux",
    "goarch": "amd64",
    "type": "Archive",
    "extra": {
      "Binary": "fleet-agent",
      "Checksum": "sha256:3333333333333333333333333333333333333333333333333333333333333333"
    }
  }
]
EOF

FLEET_ROOT_DIR="${TMP_DIR}" \
FLEET_MINISIGN_PUBLIC_KEY=$'untrusted comment: minisign public key: TESTKEY\nABC123PUBLICKEYPAYLOAD' \
  "${ROOT_DIR}/scripts/sync-signing-key.sh"

MINISIGN_SECRET_KEY="$(printf 'not-a-real-secret-key-but-valid-base64' | base64)"
export MINISIGN_SECRET_KEY
MINISIGN_PASSWORD="test-password" \
MINISIGN_PUBLIC_KEY=$'untrusted comment: minisign public key: TESTKEY\nABC123PUBLICKEYPAYLOAD' \
RELEASE_BOT_TOKEN="test-token" \
  "${ROOT_DIR}/scripts/validate-release-env.sh"

FLEET_ROOT_DIR="${TMP_DIR}" \
FLEET_MINISIGN_PUBLIC_KEY=$'untrusted comment: minisign public key: TESTKEY\nABC123PUBLICKEYPAYLOAD' \
  "${ROOT_DIR}/scripts/validate-signing-assets.sh"

grep -q 'ABC123PUBLICKEYPAYLOAD' "${TMP_DIR}/public/install.sh"
grep -q 'ABC123PUBLICKEYPAYLOAD' "${TMP_DIR}/public/install.ps1"
grep -q 'ABC123PUBLICKEYPAYLOAD' "${TMP_DIR}/public/signing.pub"
grep -q 'ABC123PUBLICKEYPAYLOAD' "${TMP_DIR}/internal/update/signing.pub"
sh -n "${TMP_DIR}/public/install"
grep -q '/install.sh' "${TMP_DIR}/public/install"
grep -q '/install.ps1' "${TMP_DIR}/public/install"

FLEET_ROOT_DIR="${TMP_DIR}" \
FLEET_DIST_DIR="${TMP_DIR}/dist" \
FLEET_VERSION="v1.2.3" \
FLEET_CHANNEL="stable" \
FLEET_RELEASE_DATE="2026-04-13T12:00:00Z" \
FLEET_RELEASE_NOTES_URL="https://github.com/cenvero/fleet/releases/tag/v1.2.3" \
FLEET_REPOSITORY="cenvero/fleet" \
  "${ROOT_DIR}/scripts/update-manifest.sh"

FLEET_ROOT_DIR="${TMP_DIR}" \
FLEET_DIST_DIR="${TMP_DIR}/dist" \
FLEET_VERSION="v1.2.3" \
FLEET_REPOSITORY="cenvero/fleet" \
  "${ROOT_DIR}/scripts/validate-release-manifest.sh"

jq -e '.channels.stable.version == "v1.2.3"' "${TMP_DIR}/public/manifest.json" >/dev/null
jq -e '.binaries["v1.2.3"]["linux-amd64"].url == "https://github.com/cenvero/fleet/releases/download/v1.2.3/fleet_v1.2.3_linux_amd64.tar.gz"' "${TMP_DIR}/public/manifest.json" >/dev/null
jq -e '.binaries["v1.2.3"]["linux-amd64"].signature_url == "https://github.com/cenvero/fleet/releases/download/v1.2.3/fleet_v1.2.3_linux_amd64.tar.gz.minisig"' "${TMP_DIR}/public/manifest.json" >/dev/null
jq -e '.binaries["v1.2.3"]["linux-armv7"].sha256 == "2222222222222222222222222222222222222222222222222222222222222222"' "${TMP_DIR}/public/manifest.json" >/dev/null
jq -e '.agent_binaries["v1.2.3"]["linux-amd64"].size > 0' "${TMP_DIR}/public/manifest.json" >/dev/null

echo "release tooling smoke test passed"
