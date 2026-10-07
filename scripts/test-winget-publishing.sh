#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# These contract assertions intentionally search for unexpanded shell source.
# shellcheck disable=SC2016
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
prepare="${ROOT_DIR}/scripts/prepare-winget-submission.sh"
submit="${ROOT_DIR}/scripts/submit-winget-release.sh"
guide="${ROOT_DIR}/docs/winget-publishing.md"

bash -n "${prepare}" "${submit}"

# PR titles follow wingetcreate/Komac: New package / New version / Add version.
title="${ROOT_DIR}/scripts/winget-pr-title.sh"
[[ "$("${title}" Cenvero.Fleet 2.4.1)" == "New package: Cenvero.Fleet version 2.4.1" ]]
[[ "$("${title}" Cenvero.Fleet 2.6.0 2.4.1)" == "New version: Cenvero.Fleet version 2.6.0" ]]
[[ "$("${title}" Cenvero.Fleet 2.10.0 2.4.1 2.9.0)" == "New version: Cenvero.Fleet version 2.10.0" ]]
[[ "$("${title}" Cenvero.Fleet 2.5.0 2.4.1 2.6.0)" == "Add version: Cenvero.Fleet version 2.5.0" ]]
[[ "$("${title}" Cenvero.Fleet 2.6.0 2.4.1 2.6.0)" == "Update version: Cenvero.Fleet version 2.6.0" ]]
grep -F 'scripts/winget-pr-title.sh' "${submit}" >/dev/null
if grep -F 'title="Update: ' "${submit}" >/dev/null; then
  echo "submit helper still uses the template-style Update: title" >&2
  exit 1
fi
"${prepare}" --help | grep -F 'read-only with respect' >/dev/null
"${submit}" --help | grep -F 'typed interactive confirmation' >/dev/null

if "${prepare}" v2.4 >/dev/null 2>&1; then
  echo "prepare helper accepted a non-stable tag" >&2
  exit 1
fi
if "${submit}" v2.4 /tmp/does-not-exist >/dev/null 2>&1; then
  echo "submit helper accepted a non-stable tag" >&2
  exit 1
fi

if ci_output="$(CI=true GH_TOKEN='' GITHUB_TOKEN='' GH_ENTERPRISE_TOKEN='' GITHUB_ENTERPRISE_TOKEN='' \
  "${submit}" v1.2.3 /tmp/does-not-exist 2>&1)"; then
  echo "submit helper accepted CI execution" >&2
  exit 1
fi
grep -F 'submission is forbidden when CI is set' <<<"${ci_output}" >/dev/null

if token_output="$(CI='' GH_TOKEN=not-a-real-token GITHUB_TOKEN='' GH_ENTERPRISE_TOKEN='' GITHUB_ENTERPRISE_TOKEN='' \
  "${submit}" v1.2.3 /tmp/does-not-exist 2>&1)"; then
  echo "submit helper accepted an environment-provided GitHub token" >&2
  exit 1
fi
grep -F 'submission refuses environment-provided GitHub credentials (GH_TOKEN)' <<<"${token_output}" >/dev/null

if grep -Eq 'WINGET_CREATE_GITHUB_TOKEN|gh auth token|--token' "${prepare}" "${submit}"; then
  echo "publishing helper contains forbidden bot-token handling" >&2
  exit 1
fi
grep -F '[[ -t 0 && -t 1 ]]' "${submit}" >/dev/null
grep -F 'SOURCE_REPOSITORY="cenvero/fleet"' "${submit}" >/dev/null
grep -F '.path == ".github/workflows/release.yml"' "${submit}" >/dev/null
grep -F 'cmp -s "${artifact_manifest}/${file_name}" "${manifest_dir}/${file_name}"' "${submit}" >/dev/null
grep -F 'rm -rf "${work_dir}/artifact"' "${prepare}" >/dev/null
if grep -F 'FLEET_WINGET_REPOSITORY' "${prepare}" "${submit}" >/dev/null; then
  echo "publishing helper permits a non-canonical Fleet source repository" >&2
  exit 1
fi
grep -F '.parent.owner.login == "microsoft"' "${submit}" >/dev/null
grep -F '.parent.name == "winget-pkgs"' "${submit}" >/dev/null
if grep -F '.parent.nameWithOwner' "${submit}" >/dev/null; then
  echo "submit helper uses an unsupported gh fork-parent field" >&2
  exit 1
fi
grep -F 'submit ${PACKAGE_IDENTIFIER} ${version}' "${submit}" >/dev/null
grep -F 'git -C "${checkout}" diff --cached' "${submit}" >/dev/null
confirmation_line="$(grep -nF '[[ "${answer}" == "${confirmation}" ]]' "${submit}" | cut -d: -f1)"
push_line="$(grep -nF 'git -C "${checkout}" push -u origin "${branch}"' "${submit}" | cut -d: -f1)"
pr_line="$(grep -nF 'pr_url="$(gh pr create' "${submit}" | cut -d: -f1)"
[[ -n "${confirmation_line}" && -n "${push_line}" && -n "${pr_line}" ]] || {
  echo "submission mutation-order contract is incomplete" >&2
  exit 1
}
((confirmation_line < push_line && push_line < pr_line)) || {
  echo "submission can mutate before exact typed confirmation" >&2
  exit 1
}
grep -F 'Do not store a classic `public_repo` token' "${guide}" >/dev/null
grep -Fi 'read this README in full' "${guide}" >/dev/null
grep -Fi 'read this README in full' "${ROOT_DIR}/README.md" >/dev/null
grep -F '[Publishing Stable Releases to WinGet]' "${ROOT_DIR}/docs/index.md" >/dev/null
grep -F 'prepare-winget-submission.sh' "${ROOT_DIR}/docs/releases-and-updates.md" >/dev/null

printf 'WinGet publishing helper checks passed\n'
