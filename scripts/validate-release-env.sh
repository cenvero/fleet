#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${MINISIGN_SECRET_KEY:?MINISIGN_SECRET_KEY is required}"
: "${MINISIGN_PASSWORD:?MINISIGN_PASSWORD is required}"
: "${MINISIGN_PUBLIC_KEY:?MINISIGN_PUBLIC_KEY is required}"
tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT INT TERM HUP

decode_base64() { if printf YQ==|base64 -d >/dev/null 2>&1; then base64 -d; else base64 -D; fi; }
printf '%s' "${MINISIGN_SECRET_KEY}"|decode_base64 >"${tmp}" 2>/dev/null || { echo "MINISIGN_SECRET_KEY must be valid base64" >&2; exit 1; }
[ -s "${tmp}" ] || { echo "MINISIGN_SECRET_KEY decoded to an empty file" >&2; exit 1; }
mode="$(stat -c '%a' "${tmp}" 2>/dev/null || stat -f '%Lp' "${tmp}")"
[ "${mode}" = 600 ] || { echo "temporary signing key must be mode 0600 (got ${mode})" >&2; exit 1; }
key_payload="$(printf '%s\n' "${MINISIGN_PUBLIC_KEY}"|awk 'NF{line=$0}END{print line}')"
[ -n "${key_payload}" ] && [ "${key_payload}" != REPLACE_WITH_MINISIGN_PUBLIC_KEY ] || { echo "MINISIGN_PUBLIC_KEY is missing or a placeholder" >&2; exit 1; }
echo "release environment looks valid"
