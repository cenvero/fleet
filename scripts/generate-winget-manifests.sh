#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: generate-winget-manifests.sh <version> <x64-url> <x64-sha256> <arm64-url> <arm64-sha256> <output-dir> [release-date]

version may be written as 2.4.1 or v2.4.1. release-date defaults to the current
UTC date and must use YYYY-MM-DD.
EOF
  exit 2
}

[ "$#" -ge 6 ] && [ "$#" -le 7 ] || usage

version="${1#v}"
x64_url="$2"
x64_sha="$(printf '%s' "$3" | tr '[:lower:]' '[:upper:]')"
arm64_url="$4"
arm64_sha="$(printf '%s' "$5" | tr '[:lower:]' '[:upper:]')"
output_dir="$6"
release_date="${7:-$(date -u +%Y-%m-%d)}"

[[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || { echo "invalid version: ${version}" >&2; exit 1; }
[[ "${release_date}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || { echo "invalid release date: ${release_date}" >&2; exit 1; }
[[ "${x64_sha}" =~ ^[0-9A-F]{64}$ ]] || { echo "invalid x64 SHA-256" >&2; exit 1; }
[[ "${arm64_sha}" =~ ^[0-9A-F]{64}$ ]] || { echo "invalid arm64 SHA-256" >&2; exit 1; }

expected_x64="https://github.com/cenvero/fleet/releases/download/v${version}/fleet_${version}_windows_amd64.zip"
expected_arm64="https://github.com/cenvero/fleet/releases/download/v${version}/fleet_${version}_windows_arm64.zip"
[ "${x64_url}" = "${expected_x64}" ] || { echo "unexpected x64 URL: ${x64_url}" >&2; echo "expected: ${expected_x64}" >&2; exit 1; }
[ "${arm64_url}" = "${expected_arm64}" ] || { echo "unexpected arm64 URL: ${arm64_url}" >&2; echo "expected: ${expected_arm64}" >&2; exit 1; }

mkdir -p "${output_dir}"

cat >"${output_dir}/Cenvero.Fleet.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.version.1.12.0.schema.json
# Created with https://github.com/microsoft/winget-create
PackageIdentifier: Cenvero.Fleet
PackageVersion: ${version}
DefaultLocale: en-US
ManifestType: version
ManifestVersion: 1.12.0
EOF

cat >"${output_dir}/Cenvero.Fleet.locale.en-US.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.defaultLocale.1.12.0.schema.json
# Created with https://github.com/microsoft/winget-create
PackageIdentifier: Cenvero.Fleet
PackageVersion: ${version}
PackageLocale: en-US
Publisher: Cenvero
PublisherUrl: https://cenvero.org
PublisherSupportUrl: https://github.com/cenvero/fleet/issues
Author: Cenvero
PackageName: Cenvero Fleet
PackageUrl: https://github.com/cenvero/fleet
License: AGPL-3.0-or-later
LicenseUrl: https://github.com/cenvero/fleet/blob/v${version}/LICENSE
Copyright: Copyright (C) 2026 Cenvero / Shubhdeep Singh
ShortDescription: Self-hosted, operator-owned fleet management controller
Description: |-
  Cenvero Fleet is a self-hosted fleet management controller for Linux, macOS,
  and Windows. It manages remote nodes over authenticated SSH-based direct and
  reverse transport modes while keeping controller data under operator control.
Moniker: fleet
Tags:
  - cli
  - devops
  - fleet-management
  - self-hosted
  - ssh
ReleaseNotesUrl: https://github.com/cenvero/fleet/releases/tag/v${version}
ManifestType: defaultLocale
ManifestVersion: 1.12.0
EOF

cat >"${output_dir}/Cenvero.Fleet.installer.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.installer.1.12.0.schema.json
# Created with https://github.com/microsoft/winget-create
PackageIdentifier: Cenvero.Fleet
PackageVersion: ${version}
Commands:
  - fleet
Installers:
  - Architecture: x64
    InstallerType: zip
    InstallerUrl: ${x64_url}
    InstallerSha256: ${x64_sha}
    NestedInstallerType: portable
    NestedInstallerFiles:
      - RelativeFilePath: fleet.exe
        PortableCommandAlias: fleet
    UpgradeBehavior: install
    ReleaseDate: ${release_date}
  - Architecture: arm64
    InstallerType: zip
    InstallerUrl: ${arm64_url}
    InstallerSha256: ${arm64_sha}
    NestedInstallerType: portable
    NestedInstallerFiles:
      - RelativeFilePath: fleet.exe
        PortableCommandAlias: fleet
    UpgradeBehavior: install
    ReleaseDate: ${release_date}
ManifestType: installer
ManifestVersion: 1.12.0
EOF

printf 'Generated WinGet manifests in %s\n' "${output_dir}"
