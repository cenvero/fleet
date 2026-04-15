#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${FLEET_ROOT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
MANIFEST_PATH="${ROOT_DIR}/public/manifest.json"
DIST_DIR="${FLEET_DIST_DIR:-${ROOT_DIR}/dist}"
ARTIFACTS_PATH="${DIST_DIR}/artifacts.json"

: "${FLEET_VERSION:?FLEET_VERSION is required}"
: "${FLEET_REPOSITORY:?FLEET_REPOSITORY is required}"

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 1
fi

[ -f "${MANIFEST_PATH}" ] || {
  echo "manifest not found: ${MANIFEST_PATH}" >&2
  exit 1
}
[ -f "${ARTIFACTS_PATH}" ] || {
  echo "artifacts.json not found: ${ARTIFACTS_PATH}" >&2
  exit 1
}

resolve_artifact_path() {
  local raw="$1"
  if [ -f "${raw}" ]; then
    printf '%s\n' "${raw}"
    return 0
  fi
  if [ -f "${ROOT_DIR}/${raw}" ]; then
    printf '%s\n' "${ROOT_DIR}/${raw}"
    return 0
  fi
  if [ -f "${DIST_DIR}/$(basename "${raw}")" ]; then
    printf '%s\n' "${DIST_DIR}/$(basename "${raw}")"
    return 0
  fi
  return 1
}

validated=0

while IFS= read -r artifact; do
  binary="$(jq -r '.binary' <<< "${artifact}")"
  name="$(jq -r '.name' <<< "${artifact}")"
  path="$(jq -r '.path' <<< "${artifact}")"
  goos="$(jq -r '.goos' <<< "${artifact}")"
  goarch="$(jq -r '.goarch' <<< "${artifact}")"
  goarm="$(jq -r '.goarm' <<< "${artifact}")"
  checksum="$(jq -r '.checksum' <<< "${artifact}")"

  case "${binary}" in
    fleet) manifest_key="binaries" ;;
    fleet-agent) manifest_key="agent_binaries" ;;
    *) continue ;;
  esac

  artifact_path="$(resolve_artifact_path "${path}")" || {
    echo "artifact path not found: ${path}" >&2
    exit 1
  }
  sha256="${checksum#sha256:}"
  [ -n "${sha256}" ] || {
    echo "checksum missing for ${name}" >&2
    exit 1
  }

  if [ "${goarch}" = "arm" ] && [ -n "${goarm}" ] && [ "${goarm}" != "null" ]; then
    target="${goos}-armv${goarm}"
  else
    target="${goos}-${goarch}"
  fi

  url="https://github.com/${FLEET_REPOSITORY}/releases/download/${FLEET_VERSION}/${name}"
  signature_url=""
  if [ -f "${artifact_path}.minisig" ]; then
    signature_url="${url}.minisig"
  fi
  size="$(wc -c < "${artifact_path}" | tr -d '[:space:]')"

  jq -e \
    --arg manifest_key "${manifest_key}" \
    --arg version "${FLEET_VERSION}" \
    --arg target "${target}" \
    --arg url "${url}" \
    --arg sha256 "${sha256}" \
    --arg signature_url "${signature_url}" \
    --argjson size "${size}" \
    '
    .[$manifest_key][$version][$target].url == $url
    and .[$manifest_key][$version][$target].sha256 == $sha256
    and ((.[$manifest_key][$version][$target].signature_url // "") == $signature_url)
    and (.[$manifest_key][$version][$target].size == $size)
    ' "${MANIFEST_PATH}" >/dev/null || {
      echo "manifest entry does not match artifact metadata for ${name}" >&2
      exit 1
    }

  validated=$((validated + 1))
done < <(
  jq -c --arg version "${FLEET_VERSION}" '
    .[]
    | select(.type == "Archive")
    | select((.extra.Binary // (.extra.Binaries[0] // "")) == "fleet" or (.extra.Binary // (.extra.Binaries[0] // "")) == "fleet-agent")
    | select(.name | contains(($version | ltrimstr("v")) + "_"))
    | {
        binary: (.extra.Binary // (.extra.Binaries[0] // "")),
        name: .name,
        path: .path,
        goos: .goos,
        goarch: .goarch,
        goarm: (.goarm // ""),
        checksum: (.extra.Checksum // "")
      }
  ' "${ARTIFACTS_PATH}"
)

[ "${validated}" -gt 0 ] || {
  echo "no release artifacts matched ${FLEET_VERSION}" >&2
  exit 1
}

echo "release manifest matches ${validated} artifact entries"
