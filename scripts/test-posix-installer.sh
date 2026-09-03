#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Cenvero / Shubhdeep Singh

set -euo pipefail

if [[ "${GITHUB_ACTIONS:-}" != "true" ]]; then
  echo "this smoke test installs into a system binary directory and must run only on a GitHub Actions runner" >&2
  exit 1
fi

for dependency in curl jq minisign; do
  command -v "${dependency}" >/dev/null 2>&1 || {
    echo "missing smoke-test dependency: ${dependency}" >&2
    exit 1
  }
done

manifest="$(curl -fsSL https://fleet.cenvero.org/manifest.json)"
expected_version="$(jq -er '.channels.stable.version | select(type == "string" and test("^v[0-9]+\\.[0-9]+\\.[0-9]+"))' <<<"${manifest}")"
expected_number="${expected_version#v}"

os="$(uname -s)"
case "${os}" in
  Linux)
    candidates=(/usr/bin/fleet)
    ;;
  Darwin)
    candidates=(/usr/local/bin/fleet /opt/homebrew/bin/fleet)
    ;;
  *)
    echo "unsupported smoke-test OS: ${os}" >&2
    exit 1
    ;;
esac

# GitHub-hosted runners are ephemeral. Remove any image-provided copy so the
# assertions below can only succeed with the binary installed by this run.
for candidate in "${candidates[@]}"; do
  sudo rm -f "${candidate}"
done

FLEET_CHANNEL=stable FLEET_VERSION="${expected_version}" sh ./public/install.sh

installed=""
for candidate in "${candidates[@]}"; do
  if [[ -x "${candidate}" ]]; then
    installed="${candidate}"
    break
  fi
done
if [[ -z "${installed}" ]]; then
  echo "installer did not create fleet in an expected system binary directory" >&2
  exit 1
fi

version_output="$("${installed}" --version)"
if [[ "${version_output}" != *"${expected_number}"* ]]; then
  echo "installed version mismatch: expected ${expected_version}, got: ${version_output}" >&2
  exit 1
fi

help_output="$("${installed}" --help)"
if [[ "${help_output}" != *"Cenvero Fleet"* ]]; then
  echo "installed fleet failed its command smoke test" >&2
  exit 1
fi

printf 'Installed and executed %s at %s on %s/%s\n' "${version_output}" "${installed}" "${os}" "$(uname -m)"
