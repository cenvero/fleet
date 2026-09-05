#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE_REPOSITORY="cenvero/fleet"
UPSTREAM_REPOSITORY="microsoft/winget-pkgs"
PACKAGE_IDENTIFIER="Cenvero.Fleet"

usage() {
  cat <<'EOF'
usage: submit-winget-release.sh <vMAJOR.MINOR.PATCH> <prepared-directory>

Revalidate a bundle created by prepare-winget-submission.sh, stage exactly three
manifest files in an existing local-account winget-pkgs fork, show the complete
diff, and require typed interactive confirmation before push and PR creation.
EOF
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

api_path_exists() {
  local endpoint="$1" response rc
  set +e
  response="$(gh api -i "${endpoint}" 2>&1)"
  rc=$?
  set -e
  if [[ ${rc} -eq 0 ]]; then
    return 0
  fi
  if grep -Eq 'HTTP[/ ]+[^ ]* ?404|HTTP 404' <<<"${response}"; then
    return 1
  fi
  printf '%s\n' "${response}" >&2
  fail "GitHub API request failed: ${endpoint}"
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
[[ $# -eq 2 ]] || { usage >&2; exit 2; }

tag="$1"
prepared="$2"
[[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "release tag must match vMAJOR.MINOR.PATCH"
version="${tag#v}"
[[ -z "${CI:-}" ]] || fail "submission is forbidden when CI is set"
for token_variable in GH_TOKEN GITHUB_TOKEN GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN; do
  [[ -z "${!token_variable:-}" ]] ||
    fail "submission refuses environment-provided GitHub credentials (${token_variable})"
done
[[ -d "${prepared}" ]] || fail "prepared directory does not exist: ${prepared}"
[[ -t 0 && -t 1 ]] || fail "submission must run in an interactive terminal"

for command_name in gh git jq minisign unzip awk sed find grep cmp; do
  require_command "${command_name}"
done
gh auth status >/dev/null
login="$(gh api user --jq '.login')"
fork_owner="${FLEET_WINGET_FORK_OWNER:-${login}}"
[[ "${fork_owner}" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$ ]] ||
  fail "fork owner is not a valid GitHub login: ${fork_owner}"
fork_repository="${fork_owner}/winget-pkgs"

gh repo view "${fork_repository}" --json isFork,parent --jq '
  select(
    .isFork == true and
    .parent.owner.login == "microsoft" and
    .parent.name == "winget-pkgs"
  ) | "\(.parent.owner.login)/\(.parent.name)"
' | grep -Fx "${UPSTREAM_REPOSITORY}" >/dev/null ||
  fail "${fork_repository} is not an existing fork of ${UPSTREAM_REPOSITORY}"

metadata="${prepared}/submission.json"
manifest_dir="${prepared}/manifests/c/Cenvero/Fleet/${version}"
x64_path="${prepared}/assets/fleet_${version}_windows_amd64.zip"
arm64_path="${prepared}/assets/fleet_${version}_windows_arm64.zip"
[[ -f "${metadata}" ]] || fail "prepared metadata is missing"
for path in \
  "${manifest_dir}/Cenvero.Fleet.yaml" \
  "${manifest_dir}/Cenvero.Fleet.locale.en-US.yaml" \
  "${manifest_dir}/Cenvero.Fleet.installer.yaml" \
  "${x64_path}" "${x64_path}.minisig" \
  "${arm64_path}" "${arm64_path}.minisig"; do
  [[ -f "${path}" ]] || fail "prepared file is missing: ${path}"
done

jq -e --arg tag "${tag}" --arg version "${version}" --arg repository "${SOURCE_REPOSITORY}" '
  .package_identifier == "Cenvero.Fleet" and
  .release_tag == $tag and
  .version == $version and
  .repository == $repository and
  (.tag_commit | test("^[0-9a-f]{40}$")) and
  (.release_url | type == "string" and length > 0) and
  (.release_run_id | type == "number" and . > 0) and
  (.release_run_url | type == "string" and length > 0) and
  (.x64_sha256 | test("^[0-9a-f]{64}$")) and
  (.arm64_sha256 | test("^[0-9a-f]{64}$"))
' "${metadata}" >/dev/null || fail "prepared metadata does not match ${tag}"

git -C "${ROOT_DIR}" fetch --quiet origin \
  "refs/heads/main:refs/remotes/origin/main" \
  "refs/tags/${tag}:refs/tags/${tag}"
tag_commit="$(git -C "${ROOT_DIR}" rev-parse "${tag}^{commit}")"
[[ "$(jq -r '.tag_commit' "${metadata}")" == "${tag_commit}" ]] || fail "prepared tag commit no longer matches ${tag}"
git -C "${ROOT_DIR}" merge-base --is-ancestor "${tag_commit}" origin/main || fail "${tag} is not on origin/main history"

release_url="$(jq -r '.release_url' "${metadata}")"
release_json="$(gh release view "${tag}" --repo "${SOURCE_REPOSITORY}" --json tagName,isDraft,isPrerelease,url)"
printf '%s\n' "${release_json}" | jq -e --arg tag "${tag}" --arg url "${release_url}" '
  .tagName == $tag and .isDraft == false and .isPrerelease == false and .url == $url
' >/dev/null || fail "prepared release provenance no longer matches ${tag}"

run_id="$(jq -r '.release_run_id' "${metadata}")"
run_json="$(gh api "repos/${SOURCE_REPOSITORY}/actions/runs/${run_id}")"
printf '%s\n' "${run_json}" | jq -e \
  --arg tag "${tag}" \
  --arg sha "${tag_commit}" \
  --argjson run_id "${run_id}" '
  .id == $run_id and
  .path == ".github/workflows/release.yml" and
  .event == "push" and
  .head_branch == $tag and
  .head_sha == $sha and
  .status == "completed" and
  .conclusion == "success"
' >/dev/null || fail "recorded workflow is not a successful release.yml run for ${tag_commit}"
run_url="$(printf '%s\n' "${run_json}" | jq -r '.html_url')"
[[ "$(jq -r '.release_run_url' "${metadata}")" == "${run_url}" ]] ||
  fail "prepared workflow URL does not match run ${run_id}"

work_root="$(mktemp -d "${TMPDIR:-/tmp}/cenvero-winget-submit.XXXXXX")"
trap 'rm -rf "${work_root}"' EXIT INT TERM
artifact_root="${work_root}/release-artifact"
mkdir -p "${artifact_root}"
gh run download "${run_id}" --repo "${SOURCE_REPOSITORY}" \
  --name "winget-manifests-${tag}" --dir "${artifact_root}"
artifact_manifest=""
for candidate in \
  "${artifact_root}/c/Cenvero/Fleet/${version}" \
  "${artifact_root}/manifests/c/Cenvero/Fleet/${version}"; do
  if [[ -d "${candidate}" ]]; then
    artifact_manifest="${candidate}"
    break
  fi
done
[[ -n "${artifact_manifest}" ]] || fail "recorded workflow artifact does not contain ${PACKAGE_IDENTIFIER} ${version}"
[[ "$(find "${artifact_manifest}" -type f | wc -l | tr -d ' ')" == "3" ]] ||
  fail "recorded workflow artifact is not the exact three-file manifest set"
for file_name in Cenvero.Fleet.yaml Cenvero.Fleet.locale.en-US.yaml Cenvero.Fleet.installer.yaml; do
  [[ -f "${artifact_manifest}/${file_name}" ]] || fail "recorded workflow artifact is missing ${file_name}"
  cmp -s "${artifact_manifest}/${file_name}" "${manifest_dir}/${file_name}" ||
    fail "prepared ${file_name} differs from recorded workflow artifact"
done
rm -rf "${artifact_root}"

public_key="$(git -C "${ROOT_DIR}" show "${tag}^{commit}:public/signing.pub" | awk 'NF{line=$0}END{print line}')"
minisign -Vm "${x64_path}" -P "${public_key}" -x "${x64_path}.minisig" >/dev/null
minisign -Vm "${arm64_path}" -P "${public_key}" -x "${arm64_path}.minisig" >/dev/null
[[ "$(sed -n '3s/^trusted comment: //p' "${x64_path}.minisig")" == "cenvero-fleet fleet ${tag} windows-amd64" ]] || fail "x64 trusted comment mismatch"
[[ "$(sed -n '3s/^trusted comment: //p' "${arm64_path}.minisig")" == "cenvero-fleet fleet ${tag} windows-arm64" ]] || fail "ARM64 trusted comment mismatch"
"${ROOT_DIR}/scripts/validate-winget-manifests.sh" "${manifest_dir}" "${x64_path}" "${arm64_path}"

version_endpoint="repos/${UPSTREAM_REPOSITORY}/contents/manifests/c/Cenvero/Fleet/${version}"
if api_path_exists "${version_endpoint}"; then
  fail "${PACKAGE_IDENTIFIER} ${version} already exists in the official repository"
fi

package_endpoint="repos/${UPSTREAM_REPOSITORY}/contents/manifests/c/Cenvero/Fleet"
if api_path_exists "${package_endpoint}"; then
  title="Update: ${PACKAGE_IDENTIFIER} to ${version}"
  branch="update-cenvero-fleet-${version//./-}"
else
  title="New package: ${PACKAGE_IDENTIFIER} version ${version}"
  branch="new-cenvero-fleet-${version//./-}"
fi

open_prs="$(gh pr list --repo "${UPSTREAM_REPOSITORY}" --state open \
  --search "${PACKAGE_IDENTIFIER} ${version} in:title,body" --json number,title,url)"
[[ "$(printf '%s\n' "${open_prs}" | jq 'length')" == "0" ]] || {
  printf '%s\n' "${open_prs}" | jq -r '.[] | "existing PR #\(.number): \(.title) \(.url)"' >&2
  fail "a matching open pull request already exists"
}

checkout="${work_root}/winget-pkgs"
git clone --depth=1 --filter=blob:none --sparse "https://github.com/${fork_repository}.git" "${checkout}"
git -C "${checkout}" remote add upstream "https://github.com/${UPSTREAM_REPOSITORY}.git"
git -C "${checkout}" fetch --depth=1 upstream master
git -C "${checkout}" sparse-checkout set --no-cone "/manifests/c/Cenvero/Fleet/${version}/" '/.github/PULL_REQUEST_TEMPLATE.md'
if git -C "${checkout}" ls-remote --exit-code --heads origin "${branch}" >/dev/null 2>&1; then
  fail "fork branch already exists: ${branch}"
fi
git -C "${checkout}" switch -c "${branch}" upstream/master

target="${checkout}/manifests/c/Cenvero/Fleet/${version}"
[[ ! -e "${target}" ]] || fail "target version path already exists in upstream master"
mkdir -p "${target}"
cp "${manifest_dir}"/*.yaml "${target}/"
git -C "${checkout}" add "manifests/c/Cenvero/Fleet/${version}"

expected_paths="$(printf '%s\n' \
  "manifests/c/Cenvero/Fleet/${version}/Cenvero.Fleet.installer.yaml" \
  "manifests/c/Cenvero/Fleet/${version}/Cenvero.Fleet.locale.en-US.yaml" \
  "manifests/c/Cenvero/Fleet/${version}/Cenvero.Fleet.yaml" | sort)"
actual_paths="$(git -C "${checkout}" diff --cached --name-only | sort)"
[[ "${actual_paths}" == "${expected_paths}" ]] || fail "staged diff is not the exact three-file manifest set"
git -C "${checkout}" diff --cached --check
[[ -n "$(git -C "${checkout}" config user.name || true)" ]] || fail "git user.name is not configured"
[[ -n "$(git -C "${checkout}" config user.email || true)" ]] || fail "git user.email is not configured"

printf '\nReady to submit %s %s\n' "${PACKAGE_IDENTIFIER}" "${version}"
printf '  authenticated account: %s\n' "${login}"
printf '  fork: %s\n' "${fork_repository}"
printf '  branch: %s\n' "${branch}"
printf '  title: %s\n\n' "${title}"
git -C "${checkout}" diff --cached --stat
git -C "${checkout}" diff --cached

confirmation="submit ${PACKAGE_IDENTIFIER} ${version}"
printf '\nType exactly "%s" to push the fork branch and open the Microsoft PR: ' "${confirmation}"
IFS= read -r answer || fail "confirmation input ended"
[[ "${answer}" == "${confirmation}" ]] || fail "submission cancelled"

git -C "${checkout}" commit -m "${title}"
git -C "${checkout}" push -u origin "${branch}"

body_file="${work_root}/pr-body.md"
cat >"${body_file}" <<EOF
## 📖 Description

Publishes \`${PACKAGE_IDENTIFIER}\` version \`${version}\` from the verified Cenvero Fleet stable release.

The prepared bundle was bound to the published x64 and ARM64 ZIPs, verified with Fleet's release minisign key and trusted comments, validated against archive contents and SHA-256 values, and exercised by the tag-triggered Windows WinGet workflow.

## ✅ Checklist

- [ ] Signed the [Contributor License Agreement](https://cla.opensource.microsoft.com) if required for this account
- [x] Linked to an issue (not applicable for this package publication)

## 📦 Manifest Checklist

- [x] Checked that there are no other open pull requests for this version
- [x] This PR only modifies one manifest version
- [x] Validated the manifest bundle and published archives locally
- [x] Tested installation and removal in the Fleet release workflow
- [x] Manifest conforms to the WinGet 1.12 schema
EOF

pr_url="$(gh pr create --repo "${UPSTREAM_REPOSITORY}" --base master \
  --head "${fork_owner}:${branch}" --title "${title}" --body-file "${body_file}")"
printf '\nOpened Microsoft WinGet pull request: %s\n' "${pr_url}"
printf 'Monitor it with: gh pr checks %s --repo %s --watch\n' "${pr_url##*/}" "${UPSTREAM_REPOSITORY}"
