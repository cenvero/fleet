#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Cenvero

# Print the microsoft/winget-pkgs pull request title for submitting <version>
# of <package-identifier>, given the versions already in the catalog.
#
# The rules match Microsoft's wingetcreate (GetPRTitle) and Komac, the tools
# that open most winget-pkgs pull requests:
#
#   no version in the catalog yet     New package: <id> version <version>
#   newer than every catalog version  New version: <id> version <version>
#   older than the newest one         Add version: <id> version <version>
#   already in the catalog            Update version: <id> version <version>
#
# usage: winget-pr-title.sh <package-identifier> <version> [catalog-version ...]

set -euo pipefail

[[ "$#" -ge 2 ]] || {
  echo "usage: winget-pr-title.sh <package-identifier> <version> [catalog-version ...]" >&2
  exit 2
}

identifier="$1"
version="$2"
shift 2

if [[ "$#" -eq 0 ]]; then
  printf 'New package: %s version %s\n' "${identifier}" "${version}"
  exit 0
fi

for existing in "$@"; do
  if [[ "${existing}" == "${version}" ]]; then
    printf 'Update version: %s version %s\n' "${identifier}" "${version}"
    exit 0
  fi
done

newest="$(printf '%s\n' "$@" "${version}" | sort -V | tail -n 1)"
if [[ "${newest}" == "${version}" ]]; then
  printf 'New version: %s version %s\n' "${identifier}" "${version}"
else
  printf 'Add version: %s version %s\n' "${identifier}" "${version}"
fi
