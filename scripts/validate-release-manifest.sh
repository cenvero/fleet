#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${FLEET_ROOT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
MANIFEST_PATH="${ROOT_DIR}/public/manifest.json"
DIST_DIR="${FLEET_DIST_DIR:-${ROOT_DIR}/dist}"
ARTIFACTS_PATH="${DIST_DIR}/artifacts.json"
EXPECTED_TARGETS=(darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 linux-armv7 windows-amd64 windows-arm64)
: "${FLEET_VERSION:?FLEET_VERSION is required}"
: "${FLEET_REPOSITORY:?FLEET_REPOSITORY is required}"
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
[ -f "${MANIFEST_PATH}" ] && [ -f "${ARTIFACTS_PATH}" ] || { echo "manifest or artifacts.json missing" >&2; exit 1; }

resolve_artifact_path() {
  local raw="$1"
  for candidate in "${raw}" "${ROOT_DIR}/${raw}" "${DIST_DIR}/$(basename "${raw}")"; do
    [ -f "${candidate}" ] && { printf '%s\n' "${candidate}"; return 0; }
  done
  return 1
}
sha256_file() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"|awk '{print $1}'; else shasum -a 256 "$1"|awk '{print $1}'; fi; }

declare -A seen
validated=0
while IFS= read -r artifact; do
  binary="$(jq -r '.binary' <<<"${artifact}")"; name="$(jq -r '.name' <<<"${artifact}")"; path="$(jq -r '.path' <<<"${artifact}")"
  goos="$(jq -r '.goos' <<<"${artifact}")"; goarch="$(jq -r '.goarch' <<<"${artifact}")"; goarm="$(jq -r '.goarm' <<<"${artifact}")"
  [[ "${binary}" == fleet || "${binary}" == fleet-agent ]] || continue
  manifest_key=binaries; [ "${binary}" = fleet-agent ] && manifest_key=agent_binaries
  if [ "${goarch}" = arm ]; then [ "${goarm}" = 7 ] || exit 1; target="${goos}-armv7"; else target="${goos}-${goarch}"; fi
  key="${binary}:${target}"; [ -z "${seen[${key}]:-}" ] || { echo "duplicate ${key}" >&2; exit 1; }; seen[${key}]=1
  artifact_path="$(resolve_artifact_path "${path}")" || { echo "artifact path missing: ${path}" >&2; exit 1; }
  signature_path="${artifact_path}.minisig"; [ -s "${signature_path}" ] || { echo "signature missing for ${name}" >&2; exit 1; }
  sha256="$(sha256_file "${artifact_path}")"; size="$(wc -c <"${artifact_path}"|tr -d '[:space:]')"
  url="https://github.com/${FLEET_REPOSITORY}/releases/download/${FLEET_VERSION}/${name}"
  trusted="$(sed -n '3s/^trusted comment: //p' "${signature_path}")"
  [ "${trusted}" = "cenvero-fleet ${binary} ${FLEET_VERSION} ${target}" ] || { echo "trusted comment mismatch for ${name}" >&2; exit 1; }

  jq -e --arg k "${manifest_key}" --arg v "${FLEET_VERSION}" --arg t "${target}" --arg u "${url}" --arg h "${sha256}" --arg s "${url}.minisig" --argjson z "${size}" '
    .[$k][$v][$t].url==$u and .[$k][$v][$t].sha256==$h and .[$k][$v][$t].signature_url==$s and .[$k][$v][$t].size==$z
  ' "${MANIFEST_PATH}" >/dev/null || { echo "manifest entry mismatch for ${name}" >&2; exit 1; }

  if [ -n "${FLEET_MINISIGN_PUBLIC_KEY:-}" ]; then
    command -v minisign >/dev/null 2>&1 || { echo "minisign is required for cryptographic validation" >&2; exit 1; }
    pub="$(printf '%s\n' "${FLEET_MINISIGN_PUBLIC_KEY}"|awk 'NF{line=$0}END{print line}')"
    minisign -Vm "${artifact_path}" -P "${pub}" -x "${signature_path}" >/dev/null 2>&1 || { echo "signature verification failed for ${name}" >&2; exit 1; }
  fi
  validated=$((validated+1))
done < <(jq -c '.[] | select(.type=="Archive" or .type=="Zip") | ((.extra.Binary // (.extra.Binaries[0] // ""))|sub("\\.exe$";"")) as $bin | select($bin=="fleet" or $bin=="fleet-agent") | {binary:$bin,name:.name,path:.path,goos:.goos,goarch:.goarch,goarm:(.goarm // "")}' "${ARTIFACTS_PATH}")

for binary in fleet fleet-agent; do for target in "${EXPECTED_TARGETS[@]}"; do
  [ -n "${seen[${binary}:${target}]:-}" ] || { echo "incomplete release matrix: missing ${binary} ${target}" >&2; exit 1; }
done; done
[ "${validated}" -eq 14 ] || { echo "expected 14 release artifacts, validated ${validated}" >&2; exit 1; }

echo "release manifest matches complete signed 14-artifact matrix"
