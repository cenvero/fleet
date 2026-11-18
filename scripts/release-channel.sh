#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Cenvero

# Print the update channel (stable, beta or alpha) a release tag publishes to.
#
#   v2.6.1          -> stable
#   v3.0.0-beta.2   -> beta    (also -beta2, -rc.1, -rc1)
#   v3.0.0-alpha.1  -> alpha   (also -alpha1)
#
# Any other pre-release suffix (v3.0.0-preview.1, v3.0.0-rc) is refused with a
# non-zero exit: a tag GoReleaser marks as a pre-release must never land in the
# stable manifest, which every installer and updater reads by default.
set -euo pipefail

tag="${1:-}"
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z.-]+))?$'
if [[ ! "${tag}" =~ ${semver} ]]; then
  echo "release-channel: ${tag:-<empty>} is not a vMAJOR.MINOR.PATCH[-PRERELEASE] tag" >&2
  exit 1
fi
pre="${BASH_REMATCH[5]}"
if [[ -z "${pre}" ]]; then
  echo stable
elif [[ "${pre}" =~ ^alpha\.?[0-9]+(\..*)?$ ]]; then
  echo alpha
elif [[ "${pre}" =~ ^(beta|rc)\.?[0-9]+(\..*)?$ ]]; then
  echo beta
else
  echo "release-channel: ${tag} has an unrecognised pre-release suffix \"-${pre}\"; use -alpha.N, -beta.N or -rc.N" >&2
  exit 1
fi
