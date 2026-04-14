#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${FLEET_ROOT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
PUBLIC_KEY_FILE="${ROOT_DIR}/public/signing.pub"
EMBEDDED_KEY_FILE="${ROOT_DIR}/internal/update/signing.pub"
INSTALL_SH="${ROOT_DIR}/public/install.sh"
INSTALL_PS1="${ROOT_DIR}/public/install.ps1"

: "${FLEET_MINISIGN_PUBLIC_KEY:?FLEET_MINISIGN_PUBLIC_KEY is required}"

key_payload="$(printf '%s\n' "${FLEET_MINISIGN_PUBLIC_KEY}" | awk 'NF { line = $0 } END { print line }')"
[ -n "${key_payload}" ] || {
  echo "unable to derive minisign public key payload" >&2
  exit 1
}
[ "${key_payload}" != "REPLACE_WITH_MINISIGN_PUBLIC_KEY" ] || {
  echo "refusing to validate placeholder minisign public key" >&2
  exit 1
}

extract_file_payload() {
  local path="$1"
  [ -f "${path}" ] || {
    echo "missing signing asset: ${path}" >&2
    exit 1
  }
  awk 'NF { line = $0 } END { print line }' "${path}"
}

public_payload="$(extract_file_payload "${PUBLIC_KEY_FILE}")"
embedded_payload="$(extract_file_payload "${EMBEDDED_KEY_FILE}")"
install_sh_payload="$(sed -n "s/^MINISIGN_PUBKEY='\(.*\)'$/\1/p" "${INSTALL_SH}")"
install_ps1_payload="$(sed -n 's/^\$MinisignPublicKey = "\(.*\)"$/\1/p' "${INSTALL_PS1}")"

for payload_name in public_payload embedded_payload install_sh_payload install_ps1_payload; do
  payload="${!payload_name}"
  [ -n "${payload}" ] || {
    echo "missing embedded minisign payload in ${payload_name}" >&2
    exit 1
  }
  [ "${payload}" != "REPLACE_WITH_MINISIGN_PUBLIC_KEY" ] || {
    echo "placeholder minisign payload still present in ${payload_name}" >&2
    exit 1
  }
  [ "${payload}" = "${key_payload}" ] || {
    echo "mismatched minisign payload in ${payload_name}" >&2
    exit 1
  }
done

echo "release signing assets are synced"
