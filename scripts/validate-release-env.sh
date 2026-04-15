#!/usr/bin/env bash
set -euo pipefail

: "${MINISIGN_SECRET_KEY:?MINISIGN_SECRET_KEY is required}"
: "${MINISIGN_PASSWORD:?MINISIGN_PASSWORD is required}"
: "${MINISIGN_PUBLIC_KEY:?MINISIGN_PUBLIC_KEY is required}"

tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT INT TERM

decode_base64() {
  if printf 'YQ==' | base64 -d >/dev/null 2>&1; then
    base64 -d
    return
  fi
  base64 -D
}

printf '%s' "${MINISIGN_SECRET_KEY}" | decode_base64 > "${tmp}" 2>/dev/null || {
  echo "MINISIGN_SECRET_KEY must be valid base64" >&2
  exit 1
}

[ -s "${tmp}" ] || {
  echo "MINISIGN_SECRET_KEY decoded to an empty file" >&2
  exit 1
}

key_payload="$(printf '%s\n' "${MINISIGN_PUBLIC_KEY}" | awk 'NF { line = $0 } END { print line }')"
[ -n "${key_payload}" ] || {
  echo "MINISIGN_PUBLIC_KEY must include a minisign public key payload" >&2
  exit 1
}
[ "${key_payload}" != "REPLACE_WITH_MINISIGN_PUBLIC_KEY" ] || {
  echo "MINISIGN_PUBLIC_KEY cannot be the placeholder payload" >&2
  exit 1
}

echo "release environment looks valid"
