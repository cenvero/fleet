#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${FLEET_ROOT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
MANIFEST_PATH="${ROOT_DIR}/public/manifest.json"
DIST_DIR="${FLEET_DIST_DIR:-${ROOT_DIR}/dist}"
ARTIFACTS_PATH="${DIST_DIR}/artifacts.json"
EXPECTED_TARGETS=(darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 linux-armv7 windows-amd64 windows-arm64)

: "${FLEET_VERSION:?FLEET_VERSION is required}"
: "${FLEET_CHANNEL:?FLEET_CHANNEL is required}"
: "${FLEET_RELEASE_DATE:?FLEET_RELEASE_DATE is required}"
: "${FLEET_RELEASE_NOTES_URL:?FLEET_RELEASE_NOTES_URL is required}"
FLEET_REPOSITORY="${FLEET_REPOSITORY:-${GITHUB_REPOSITORY:-}}"
: "${FLEET_REPOSITORY:?FLEET_REPOSITORY is required}"
[[ "${FLEET_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || { echo "invalid FLEET_VERSION" >&2; exit 1; }
[[ "${FLEET_REPOSITORY}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { echo "invalid FLEET_REPOSITORY" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
[ -f "${MANIFEST_PATH}" ] || { echo "manifest not found: ${MANIFEST_PATH}" >&2; exit 1; }
[ -f "${ARTIFACTS_PATH}" ] || { echo "artifacts.json not found: ${ARTIFACTS_PATH}" >&2; exit 1; }

resolve_artifact_path() {
  local raw="$1"
  for candidate in "${raw}" "${ROOT_DIR}/${raw}" "${DIST_DIR}/$(basename "${raw}")"; do
    [ -f "${candidate}" ] && { printf '%s\n' "${candidate}"; return 0; }
  done
  return 1
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'
  fi
}

fleet_entries='{}'; agent_entries='{}'
declare -A seen
version_no_v="${FLEET_VERSION#v}"

while IFS= read -r artifact; do
  binary="$(jq -r '.binary' <<<"${artifact}")"
  name="$(jq -r '.name' <<<"${artifact}")"
  path="$(jq -r '.path' <<<"${artifact}")"
  goos="$(jq -r '.goos' <<<"${artifact}")"
  goarch="$(jq -r '.goarch' <<<"${artifact}")"
  goarm="$(jq -r '.goarm' <<<"${artifact}")"
  declared_checksum="$(jq -r '.checksum' <<<"${artifact}")"
  [[ "${binary}" == fleet || "${binary}" == fleet-agent ]] || continue

  if [[ "${goarch}" == arm ]]; then
    [ "${goarm}" = 7 ] || { echo "unsupported arm release for ${name}" >&2; exit 1; }
    target="${goos}-armv7"
  else target="${goos}-${goarch}"
  fi
  expected_ext=tar.gz; [ "${goos}" = windows ] && expected_ext=zip
  expected_name="${binary}_${version_no_v}_${goos}_${target#*-}.${expected_ext}"
  [ "${name}" = "${expected_name}" ] || { echo "artifact name is not bound to release metadata: ${name} (expected ${expected_name})" >&2; exit 1; }
  key="${binary}:${target}"
  [ -z "${seen[${key}]:-}" ] || { echo "duplicate release artifact for ${key}" >&2; exit 1; }
  seen[${key}]=1

  artifact_path="$(resolve_artifact_path "${path}")" || { echo "artifact path not found: ${path}" >&2; exit 1; }
  sha256="$(sha256_file "${artifact_path}")"
  [ "${declared_checksum}" = "sha256:${sha256}" ] || { echo "artifact checksum metadata mismatch for ${name}" >&2; exit 1; }
  signature_path="${artifact_path}.minisig"
  [ -s "${signature_path}" ] || { echo "mandatory minisign signature missing for ${name}" >&2; exit 1; }
  trusted="$(sed -n '3s/^trusted comment: //p' "${signature_path}")"
  expected_trusted="cenvero-fleet ${binary} ${FLEET_VERSION} ${target}"
  [ "${trusted}" = "${expected_trusted}" ] || { echo "signature trusted comment mismatch for ${name}" >&2; exit 1; }

  size="$(wc -c <"${artifact_path}" | tr -d '[:space:]')"
  url="https://github.com/${FLEET_REPOSITORY}/releases/download/${FLEET_VERSION}/${name}"
  info="$(jq -cn --arg url "${url}" --arg sha256 "${sha256}" --arg signature_url "${url}.minisig" --argjson size "${size}" \
    '{url:$url,sha256:$sha256,signature_url:$signature_url,size:$size}')"
  if [ "${binary}" = fleet ]; then
    fleet_entries="$(jq -cn --argjson c "${fleet_entries}" --arg t "${target}" --argjson i "${info}" '$c+{($t):$i}')"
  else
    agent_entries="$(jq -cn --argjson c "${agent_entries}" --arg t "${target}" --argjson i "${info}" '$c+{($t):$i}')"
  fi
done < <(jq -c '.[] | select(.type == "Archive" or .type == "Zip") | ((.extra.Binary // (.extra.Binaries[0] // "")) | sub("\\.exe$"; "")) as $bin | select($bin == "fleet" or $bin == "fleet-agent") | {binary:$bin,name:.name,path:.path,goos:.goos,goarch:.goarch,goarm:(.goarm // ""),checksum:(.extra.Checksum // "")}' "${ARTIFACTS_PATH}")

for binary in fleet fleet-agent; do
  for target in "${EXPECTED_TARGETS[@]}"; do
    [ -n "${seen[${binary}:${target}]:-}" ] || { echo "incomplete release matrix: missing ${binary} ${target}" >&2; exit 1; }
  done
done
[ "${#seen[@]}" -eq 14 ] || { echo "unexpected release matrix size: ${#seen[@]}" >&2; exit 1; }

tmp="$(mktemp)"; trap 'rm -f "${tmp}"' EXIT INT TERM
jq --arg channel "${FLEET_CHANNEL}" --arg version "${FLEET_VERSION}" --arg release_date "${FLEET_RELEASE_DATE}" \
  --arg release_notes "${FLEET_RELEASE_NOTES_URL}" --argjson binaries "${fleet_entries}" --argjson agent_binaries "${agent_entries}" '
  .generated_at=$release_date
  | .channels[$channel].version=$version
  | .channels[$channel].release_date=$release_date
  | .channels[$channel].release_notes_url=$release_notes
  | .channels[$channel].history=([(.channels[$channel].history // [])[] | select(. != $version)]+[$version] | .[-10:])
  | .channels[$channel].min_supported=.channels[$channel].history[0]
  | .binaries[$version]=$binaries | .agent_binaries[$version]=$agent_binaries
  | ([.channels|to_entries[].value.history[]?]|unique) as $active
  | .binaries=(.binaries|with_entries(select(.key as $k|any($active[];.==$k))))
  | .agent_binaries=(.agent_binaries|with_entries(select(.key as $k|any($active[];.==$k))))
' "${MANIFEST_PATH}" >"${tmp}"
mv "${tmp}" "${MANIFEST_PATH}"
trap - EXIT INT TERM
echo "Updated ${MANIFEST_PATH} for ${FLEET_CHANNEL} ${FLEET_VERSION} (14 signed artifacts)"
