#!/usr/bin/env bash
set -euo pipefail

[ "$#" -eq 3 ] || { echo "usage: $0 <artifact> <signature> <secret-key>" >&2; exit 2; }
artifact="$1"
signature="$2"
secret_key="$3"
name="$(basename -- "${artifact}")"

if [[ "${name}" =~ ^(fleet|fleet-agent)_([^_]+)_(linux|darwin|windows)_(amd64|arm64|armv7)\.(tar\.gz|zip)$ ]]; then
  product="${BASH_REMATCH[1]}"
  version="${BASH_REMATCH[2]}"
  target="${BASH_REMATCH[3]}-${BASH_REMATCH[4]}"
else
  echo "refusing to sign artifact with an unrecognized release name: ${name}" >&2
  exit 1
fi

[[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || {
  echo "refusing to sign artifact with an invalid version: ${version}" >&2
  exit 1
}
[ -f "${artifact}" ] || { echo "artifact not found: ${artifact}" >&2; exit 1; }
[ -f "${secret_key}" ] || { echo "minisign secret key not found" >&2; exit 1; }
key_mode="$(stat -c '%a' "${secret_key}" 2>/dev/null || stat -f '%Lp' "${secret_key}")"
[[ "${key_mode}" == 600 || "${key_mode}" == 400 ]] || {
  echo "refusing minisign secret key with mode ${key_mode}; require 0600 or 0400" >&2
  exit 1
}
command -v minisign >/dev/null 2>&1 || { echo "minisign is required" >&2; exit 1; }

trusted_comment="cenvero-fleet ${product} v${version} ${target}"
exec minisign -Sm "${artifact}" -s "${secret_key}" -x "${signature}" -t "${trusted_comment}"
