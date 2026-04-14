#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${FLEET_ROOT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
PUBLIC_KEY_FILE="${ROOT_DIR}/public/signing.pub"
EMBEDDED_KEY_FILE="${ROOT_DIR}/internal/update/signing.pub"
INSTALL_SH="${ROOT_DIR}/public/install.sh"
INSTALL_PS1="${ROOT_DIR}/public/install.ps1"

: "${FLEET_MINISIGN_PUBLIC_KEY:?FLEET_MINISIGN_PUBLIC_KEY is required}"

key_text="$(printf '%s\n' "${FLEET_MINISIGN_PUBLIC_KEY}" | sed '/^[[:space:]]*$/d')"
key_payload="$(printf '%s\n' "${key_text}" | awk 'NF { line = $0 } END { print line }')"

[ -n "${key_payload}" ] || {
  echo "unable to derive minisign public key payload" >&2
  exit 1
}
[ "${key_payload}" != "REPLACE_WITH_MINISIGN_PUBLIC_KEY" ] || {
  echo "refusing to sync placeholder minisign public key" >&2
  exit 1
}

printf '%s\n' "${key_text}" > "${PUBLIC_KEY_FILE}"
printf '%s\n' "${key_text}" > "${EMBEDDED_KEY_FILE}"

tmp="$(mktemp)"
awk -v payload="${key_payload}" '
  /^MINISIGN_PUBKEY=/ { print "MINISIGN_PUBKEY='\''" payload "'\''"; next }
  { print }
' "${INSTALL_SH}" > "${tmp}"
mv "${tmp}" "${INSTALL_SH}"

tmp="$(mktemp)"
awk -v payload="${key_payload}" '
  /^\$MinisignPublicKey = / { print "$MinisignPublicKey = \"" payload "\""; next }
  { print }
' "${INSTALL_PS1}" > "${tmp}"
mv "${tmp}" "${INSTALL_PS1}"

echo "Synced minisign public key into release assets"
