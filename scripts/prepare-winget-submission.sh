#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPOSITORY="cenvero/fleet"

usage() {
  cat <<'EOF'
usage: prepare-winget-submission.sh <vMAJOR.MINOR.PATCH> [output-directory]

Download and verify the successful stable release's generated WinGet manifests,
Windows archives, and minisign sidecars. This command is read-only with respect
to GitHub and refuses to overwrite an existing output path.
EOF
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
[[ $# -ge 1 && $# -le 2 ]] || { usage >&2; exit 2; }

tag="$1"
[[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "release tag must match vMAJOR.MINOR.PATCH"
version="${tag#v}"
output="${2:-${TMPDIR:-/tmp}/cenvero-fleet-winget-${tag}}"
[[ ! -e "${output}" ]] || fail "output already exists: ${output}"

for command_name in gh git jq minisign unzip awk sed find; do
  require_command "${command_name}"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  fail "sha256sum or shasum is required"
fi

git -C "${ROOT_DIR}" fetch --quiet origin \
  "refs/heads/main:refs/remotes/origin/main" \
  "refs/tags/${tag}:refs/tags/${tag}"
tag_commit="$(git -C "${ROOT_DIR}" rev-parse "${tag}^{commit}")"
git -C "${ROOT_DIR}" merge-base --is-ancestor "${tag_commit}" origin/main ||
  fail "${tag} is not on origin/main history"

release_json="$(gh release view "${tag}" --repo "${REPOSITORY}" --json tagName,isDraft,isPrerelease,url,publishedAt)"
printf '%s\n' "${release_json}" | jq -e --arg tag "${tag}" '
  .tagName == $tag and .isDraft == false and .isPrerelease == false
' >/dev/null || fail "${tag} is not a published stable release"

runs_json="$(gh run list --repo "${REPOSITORY}" --workflow release.yml --branch "${tag}" --event push --limit 20 \
  --json databaseId,headBranch,headSha,status,conclusion,url)"
run_id="$(printf '%s\n' "${runs_json}" | jq -r --arg tag "${tag}" --arg sha "${tag_commit}" '
  [.[] | select(
    .headBranch == $tag and
    .headSha == $sha and
    .status == "completed" and
    .conclusion == "success"
  )][0].databaseId // empty
')"
[[ -n "${run_id}" ]] || fail "no successful release workflow found for ${tag} at ${tag_commit}"
run_url="$(printf '%s\n' "${runs_json}" | jq -r --argjson id "${run_id}" '.[] | select(.databaseId == $id) | .url')"

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/cenvero-winget-prepare.XXXXXX")"
trap 'rm -rf "${work_dir}"' EXIT INT TERM
mkdir -p "${work_dir}/artifact" "${work_dir}/assets" "${work_dir}/manifests/c/Cenvero/Fleet/${version}"

gh run download "${run_id}" --repo "${REPOSITORY}" \
  --name "winget-manifests-${tag}" --dir "${work_dir}/artifact"

artifact_manifest=""
for candidate in \
  "${work_dir}/artifact/c/Cenvero/Fleet/${version}" \
  "${work_dir}/artifact/manifests/c/Cenvero/Fleet/${version}"; do
  if [[ -d "${candidate}" ]]; then
    artifact_manifest="${candidate}"
    break
  fi
done
[[ -n "${artifact_manifest}" ]] || fail "artifact does not contain Cenvero.Fleet ${version} manifests"

manifest_dir="${work_dir}/manifests/c/Cenvero/Fleet/${version}"
for file_name in Cenvero.Fleet.yaml Cenvero.Fleet.locale.en-US.yaml Cenvero.Fleet.installer.yaml; do
  [[ -f "${artifact_manifest}/${file_name}" ]] || fail "artifact is missing ${file_name}"
  cp "${artifact_manifest}/${file_name}" "${manifest_dir}/${file_name}"
done
[[ "$(find "${artifact_manifest}" -type f | wc -l | tr -d ' ')" == "3" ]] ||
  fail "artifact contains files beyond the required three manifests"

x64_name="fleet_${version}_windows_amd64.zip"
arm64_name="fleet_${version}_windows_arm64.zip"
gh release download "${tag}" --repo "${REPOSITORY}" --dir "${work_dir}/assets" \
  --pattern "${x64_name}" --pattern "${x64_name}.minisig" \
  --pattern "${arm64_name}" --pattern "${arm64_name}.minisig"

x64_path="${work_dir}/assets/${x64_name}"
arm64_path="${work_dir}/assets/${arm64_name}"
for path in "${x64_path}" "${x64_path}.minisig" "${arm64_path}" "${arm64_path}.minisig"; do
  [[ -f "${path}" ]] || fail "release asset is missing: $(basename "${path}")"
done

public_key="$(git -C "${ROOT_DIR}" show "${tag}^{commit}:public/signing.pub" | awk 'NF{line=$0}END{print line}')"
[[ -n "${public_key}" ]] || fail "release tag does not contain a minisign public key"
minisign -Vm "${x64_path}" -P "${public_key}" -x "${x64_path}.minisig" >/dev/null
minisign -Vm "${arm64_path}" -P "${public_key}" -x "${arm64_path}.minisig" >/dev/null
[[ "$(sed -n '3s/^trusted comment: //p' "${x64_path}.minisig")" == "cenvero-fleet fleet ${tag} windows-amd64" ]] ||
  fail "x64 signature trusted comment does not match ${tag}"
[[ "$(sed -n '3s/^trusted comment: //p' "${arm64_path}.minisig")" == "cenvero-fleet fleet ${tag} windows-arm64" ]] ||
  fail "ARM64 signature trusted comment does not match ${tag}"

"${ROOT_DIR}/scripts/validate-winget-manifests.sh" "${manifest_dir}" "${x64_path}" "${arm64_path}"

x64_sha256="$(sha256_file "${x64_path}")"
arm64_sha256="$(sha256_file "${arm64_path}")"
jq -n \
  --arg package_identifier "Cenvero.Fleet" \
  --arg release_tag "${tag}" \
  --arg version "${version}" \
  --arg repository "${REPOSITORY}" \
  --arg tag_commit "${tag_commit}" \
  --arg release_url "$(printf '%s\n' "${release_json}" | jq -r '.url')" \
  --arg release_run_id "${run_id}" \
  --arg release_run_url "${run_url}" \
  --arg x64_sha256 "${x64_sha256}" \
  --arg arm64_sha256 "${arm64_sha256}" \
  '{
    package_identifier: $package_identifier,
    release_tag: $release_tag,
    version: $version,
    repository: $repository,
    tag_commit: $tag_commit,
    release_url: $release_url,
    release_run_id: ($release_run_id | tonumber),
    release_run_url: $release_run_url,
    x64_sha256: $x64_sha256,
    arm64_sha256: $arm64_sha256
  }' >"${work_dir}/submission.json"

mkdir -p "$(dirname "${output}")"
rm -rf "${work_dir}/artifact"
[[ ! -e "${output}" ]] || fail "output appeared while preparing submission: ${output}"
mv "${work_dir}" "${output}"
trap - EXIT INT TERM

printf '\nPrepared verified WinGet submission: %s\n' "${output}"
printf '  release: %s\n' "${tag}"
printf '  workflow: %s\n' "${run_url}"
printf '  manifests: %s\n' "${output}/manifests/c/Cenvero/Fleet/${version}"
printf '  x64 sha256: %s\n' "${x64_sha256}"
printf '  arm64 sha256: %s\n' "${arm64_sha256}"
printf '\nReview submission.json and all three manifests before running submit-winget-release.sh.\n'
