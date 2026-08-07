#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${FLEET_ROOT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
PUBLIC_KEY_FILE="${ROOT_DIR}/public/signing.pub"
EMBEDDED_KEY_FILE="${ROOT_DIR}/internal/update/signing.pub"
INSTALL_SH="${ROOT_DIR}/public/install.sh"
INSTALL_PS1="${ROOT_DIR}/public/install.ps1"
: "${FLEET_MINISIGN_PUBLIC_KEY:?FLEET_MINISIGN_PUBLIC_KEY is required}"

key_text="$(printf '%s\n' "${FLEET_MINISIGN_PUBLIC_KEY}"|sed '/^[[:space:]]*$/d')"
key_payload="$(printf '%s\n' "${key_text}"|awk 'NF{line=$0}END{print line}')"
[ -n "${key_payload}" ] && [ "${key_payload}" != REPLACE_WITH_MINISIGN_PUBLIC_KEY ] || { echo "invalid minisign public key" >&2; exit 1; }

printf '%s\n' "${key_text}" >"${PUBLIC_KEY_FILE}"
printf '%s\n' "${key_text}" >"${EMBEDDED_KEY_FILE}"
chmod 0644 "${PUBLIC_KEY_FILE}" "${EMBEDDED_KEY_FILE}"

tmp="$(mktemp)"; trap 'rm -f "${tmp}"' EXIT INT TERM
awk -v payload="${key_payload}" '/^MINISIGN_PUBKEY=/{print "MINISIGN_PUBKEY='\''" payload "'\''";next}{print}' "${INSTALL_SH}" >"${tmp}"
cat "${tmp}" >"${INSTALL_SH}"; chmod 0755 "${INSTALL_SH}"
awk -v payload="${key_payload}" '/^\$MinisignPublicKey = /{print "$MinisignPublicKey = \"" payload "\"";next}{print}' "${INSTALL_PS1}" >"${tmp}"
cat "${tmp}" >"${INSTALL_PS1}"; chmod 0644 "${INSTALL_PS1}"
rm -f "${tmp}"; trap - EXIT INT TERM
echo "Synced minisign public key into release assets"
