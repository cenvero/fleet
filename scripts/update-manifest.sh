#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${FLEET_ROOT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
MANIFEST_PATH="${ROOT_DIR}/public/manifest.json"
DIST_DIR="${FLEET_DIST_DIR:-${ROOT_DIR}/dist}"
ARTIFACTS_PATH="${DIST_DIR}/artifacts.json"

: "${FLEET_VERSION:?FLEET_VERSION is required}"
: "${FLEET_CHANNEL:?FLEET_CHANNEL is required}"
: "${FLEET_RELEASE_DATE:?FLEET_RELEASE_DATE is required}"
: "${FLEET_RELEASE_NOTES_URL:?FLEET_RELEASE_NOTES_URL is required}"

FLEET_REPOSITORY="${FLEET_REPOSITORY:-${GITHUB_REPOSITORY:-}}"
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

fleet_entries='{}'
agent_entries='{}'
fleet_count=0
agent_count=0

while IFS= read -r artifact; do
  binary="$(jq -r '.binary' <<< "${artifact}")"
  name="$(jq -r '.name' <<< "${artifact}")"
  path="$(jq -r '.path' <<< "${artifact}")"
  goos="$(jq -r '.goos' <<< "${artifact}")"
  goarch="$(jq -r '.goarch' <<< "${artifact}")"
  goarm="$(jq -r '.goarm' <<< "${artifact}")"
  checksum="$(jq -r '.checksum' <<< "${artifact}")"

  case "${binary}" in
    fleet|fleet-agent) ;;
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

  size="$(wc -c < "${artifact_path}" | tr -d '[:space:]')"
  url="https://github.com/${FLEET_REPOSITORY}/releases/download/${FLEET_VERSION}/${name}"
  signature_url=""
  if [ -f "${artifact_path}.minisig" ]; then
    signature_url="${url}.minisig"
  fi

  info="$(
    jq -cn \
      --arg url "${url}" \
      --arg sha256 "${sha256}" \
      --arg signature_url "${signature_url}" \
      --argjson size "${size}" \
      '{
        url: $url,
        sha256: $sha256,
        signature_url: $signature_url,
        size: $size
      }'
  )"

  if [ "${binary}" = "fleet" ]; then
    fleet_entries="$(
      jq -cn --argjson current "${fleet_entries}" --arg target "${target}" --argjson info "${info}" \
        '$current + {($target): $info}'
    )"
    fleet_count=$((fleet_count + 1))
  else
    agent_entries="$(
      jq -cn --argjson current "${agent_entries}" --arg target "${target}" --argjson info "${info}" \
        '$current + {($target): $info}'
    )"
    agent_count=$((agent_count + 1))
  fi
done < <(
  jq -c --arg version "${FLEET_VERSION}" '
    .[]
    | select(.type == "Archive" or .type == "Zip")
    | ((.extra.Binary // (.extra.Binaries[0] // "")) | gsub("\\.exe$"; "")) as $bin
    | select($bin == "fleet" or $bin == "fleet-agent")
    | select(.name | contains(($version | ltrimstr("v")) + "_"))
    | {
        binary: $bin,
        name: .name,
        path: .path,
        goos: .goos,
        goarch: .goarch,
        goarm: (.goarm // ""),
        checksum: (.extra.Checksum // "")
      }
  ' "${ARTIFACTS_PATH}"
)

[ "${fleet_count}" -gt 0 ] || {
  echo "no fleet archives found in ${ARTIFACTS_PATH}" >&2
  exit 1
}
[ "${agent_count}" -gt 0 ] || {
  echo "no fleet-agent archives found in ${ARTIFACTS_PATH}" >&2
  exit 1
}

tmp="$(mktemp)"

jq \
  --arg channel "${FLEET_CHANNEL}" \
  --arg version "${FLEET_VERSION}" \
  --arg release_date "${FLEET_RELEASE_DATE}" \
  --arg release_notes "${FLEET_RELEASE_NOTES_URL}" \
  --argjson binaries "${fleet_entries}" \
  --argjson agent_binaries "${agent_entries}" \
  '
  .generated_at = $release_date
  | .channels[$channel].version = $version
  | .channels[$channel].release_date = $release_date
  | .channels[$channel].release_notes_url = $release_notes
  | .channels[$channel].history = (
      [(.channels[$channel].history // []) | .[] | select(. != $version)] + [$version]
      | .[-10:]
    )
  | .channels[$channel].min_supported = .channels[$channel].history[0]
  | .binaries[$version] = $binaries
  | .agent_binaries[$version] = $agent_binaries
  | ([.channels | to_entries[] | .value.history // [] | .[]] | unique) as $active
  | .binaries = (.binaries | with_entries(select(.key as $k | any($active[]; . == $k))))
  | .agent_binaries = (.agent_binaries | with_entries(select(.key as $k | any($active[]; . == $k))))
  ' "${MANIFEST_PATH}" > "${tmp}"

mv "${tmp}" "${MANIFEST_PATH}"
echo "Updated ${MANIFEST_PATH} for ${FLEET_CHANNEL} ${FLEET_VERSION}"
