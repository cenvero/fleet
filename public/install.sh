#!/bin/sh
set -eu

BASE_URL="https://fleet.cenvero.org"
CHANNEL="${FLEET_CHANNEL:-stable}"
FLEET_VERSION="${FLEET_VERSION:-}"
MINISIGN_PUBKEY='RWRb53p9WTsWCO2RZT3bvjrZw4QjXnIo2R7NUqhPsfvhR8u0sS55hZb3'

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing dependency: $1" >&2
    exit 1
  }
}

need curl
need tar
need jq

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  armv7l) ARCH="armv7" ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

TARGET="${OS}-${ARCH}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

MANIFEST_PATH="${TMP_DIR}/manifest.json"
curl -fsSL "${BASE_URL}/manifest.json" -o "${MANIFEST_PATH}"

if [ -n "${FLEET_VERSION}" ]; then
  VERSION="${FLEET_VERSION}"
else
  VERSION="$(jq -r --arg channel "$CHANNEL" '.channels[$channel].version // empty' "${MANIFEST_PATH}")"
  [ -n "${VERSION}" ] || { echo "channel not found: ${CHANNEL}" >&2; exit 1; }
fi

URL="$(jq -r --arg version "$VERSION" --arg target "$TARGET" '.binaries[$version][$target].url // empty' "${MANIFEST_PATH}")"
SIG_URL="$(jq -r --arg version "$VERSION" --arg target "$TARGET" '.binaries[$version][$target].signature_url // empty' "${MANIFEST_PATH}")"
SHA="$(jq -r --arg version "$VERSION" --arg target "$TARGET" '.binaries[$version][$target].sha256 // empty' "${MANIFEST_PATH}")"

if [ -z "${URL}" ]; then
  if [ -n "${FLEET_VERSION}" ]; then
    URL="https://github.com/cenvero/fleet/releases/download/${VERSION}/fleet_${VERSION}_${OS}_${ARCH}.tar.gz"
    SIG_URL="${URL}.minisig"
    SHA=""
  else
    echo "target not published yet: ${TARGET}" >&2
    exit 1
  fi
fi

ARCHIVE_PATH="${TMP_DIR}/fleet.tar.gz"
curl -fsSL "${URL}" -o "${ARCHIVE_PATH}"

if [ -n "${SIG_URL}" ] && [ "${SIG_URL}" != "null" ]; then
  curl -fsSL "${SIG_URL}" -o "${TMP_DIR}/fleet.minisig"
  if command -v minisign >/dev/null 2>&1; then
    [ "${MINISIGN_PUBKEY}" != "REPLACE_WITH_MINISIGN_PUBLIC_KEY" ] || {
      echo "installer public key placeholder has not been replaced yet" >&2
      exit 1
    }
    printf '%s\n' "untrusted comment: Cenvero Fleet" "${MINISIGN_PUBKEY}" > "${TMP_DIR}/signing.pub"
    minisign -Vm "${ARCHIVE_PATH}" -P "${MINISIGN_PUBKEY}" -x "${TMP_DIR}/fleet.minisig"
  else
    echo "warning: minisign is not installed; signature verification skipped" >&2
  fi
fi

if [ -n "${SHA}" ] && [ "${SHA}" != "null" ]; then
  ACTUAL_SHA="$(shasum -a 256 "${ARCHIVE_PATH}" | awk '{print $1}')"
  [ "${ACTUAL_SHA}" = "${SHA}" ] || {
    echo "checksum mismatch" >&2
    exit 1
  }
fi

INSTALL_DIR="/usr/local/bin"
[ -w "${INSTALL_DIR}" ] || INSTALL_DIR="${HOME}/.local/bin"
mkdir -p "${INSTALL_DIR}"

tar -xzf "${ARCHIVE_PATH}" -C "${TMP_DIR}"
SOURCE_FILE="$(find "${TMP_DIR}" -type f \( -name 'fleet' -o -name 'fleet.exe' \) | head -n 1)"
[ -n "${SOURCE_FILE}" ] || {
  echo "fleet binary not found in archive" >&2
  exit 1
}
cp "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
chmod +x "${INSTALL_DIR}/fleet"

echo "Installed Cenvero Fleet ${VERSION} to ${INSTALL_DIR}/fleet"
echo "Run: fleet init"
