#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT INT TERM

version=9.8.7
x64_url="https://github.com/cenvero/fleet/releases/download/v${version}/fleet_${version}_windows_amd64.zip"
arm64_url="https://github.com/cenvero/fleet/releases/download/v${version}/fleet_${version}_windows_arm64.zip"
x64_sha="$(printf 'a%.0s' {1..64})"
arm64_sha="$(printf 'b%.0s' {1..64})"

"${ROOT_DIR}/scripts/generate-winget-manifests.sh" \
  "v${version}" "${x64_url}" "${x64_sha}" "${arm64_url}" "${arm64_sha}" \
  "${TMP_DIR}/manifests" 2026-09-05 >/dev/null

version_file="${TMP_DIR}/manifests/Cenvero.Fleet.yaml"
locale_file="${TMP_DIR}/manifests/Cenvero.Fleet.locale.en-US.yaml"
installer_file="${TMP_DIR}/manifests/Cenvero.Fleet.installer.yaml"
for file in "${version_file}" "${locale_file}" "${installer_file}"; do
  [ -s "${file}" ] || { echo "missing generated manifest: ${file}" >&2; exit 1; }
  grep -Fx 'PackageIdentifier: Cenvero.Fleet' "${file}" >/dev/null
  grep -Fx 'PackageVersion: 9.8.7' "${file}" >/dev/null
  grep -Fx 'ManifestVersion: 1.12.0' "${file}" >/dev/null
done

grep -Fx '# yaml-language-server: $schema=https://aka.ms/winget-manifest.version.1.12.0.schema.json' "${version_file}" >/dev/null
grep -Fx '# yaml-language-server: $schema=https://aka.ms/winget-manifest.defaultLocale.1.12.0.schema.json' "${locale_file}" >/dev/null
grep -Fx '# yaml-language-server: $schema=https://aka.ms/winget-manifest.installer.1.12.0.schema.json' "${installer_file}" >/dev/null
grep -Fx 'DefaultLocale: en-US' "${version_file}" >/dev/null
grep -Fx 'License: AGPL-3.0-or-later' "${locale_file}" >/dev/null
grep -Fx '  - Architecture: x64' "${installer_file}" >/dev/null
grep -Fx '  - Architecture: arm64' "${installer_file}" >/dev/null
[ "$(grep -Fc '    InstallerType: zip' "${installer_file}")" -eq 2 ]
[ "$(grep -Fc '    NestedInstallerType: portable' "${installer_file}")" -eq 2 ]
[ "$(grep -Fc '      - RelativeFilePath: fleet.exe' "${installer_file}")" -eq 2 ]
[ "$(grep -Fc '        PortableCommandAlias: fleet' "${installer_file}")" -eq 2 ]
grep -Fx "    InstallerUrl: ${x64_url}" "${installer_file}" >/dev/null
grep -Fx "    InstallerUrl: ${arm64_url}" "${installer_file}" >/dev/null
grep -Fx "    InstallerSha256: $(printf '%s' "${x64_sha}" | tr '[:lower:]' '[:upper:]')" "${installer_file}" >/dev/null
grep -Fx "    InstallerSha256: $(printf '%s' "${arm64_sha}" | tr '[:lower:]' '[:upper:]')" "${installer_file}" >/dev/null
"${ROOT_DIR}/scripts/validate-winget-manifests.sh" "${TMP_DIR}/manifests" >/dev/null

command -v zip >/dev/null 2>&1 || { echo "zip is required for WinGet manifest tests" >&2; exit 1; }
command -v unzip >/dev/null 2>&1 || { echo "unzip is required for WinGet manifest tests" >&2; exit 1; }
mkdir -p "${TMP_DIR}/x64" "${TMP_DIR}/arm64"
printf 'x64 fleet\n' >"${TMP_DIR}/x64/fleet.exe"
printf 'arm64 fleet\n' >"${TMP_DIR}/arm64/fleet.exe"
(cd "${TMP_DIR}/x64" && zip -q "${TMP_DIR}/fleet_${version}_windows_amd64.zip" fleet.exe)
(cd "${TMP_DIR}/arm64" && zip -q "${TMP_DIR}/fleet_${version}_windows_arm64.zip" fleet.exe)
if command -v sha256sum >/dev/null 2>&1; then
  actual_x64_sha="$(sha256sum "${TMP_DIR}/fleet_${version}_windows_amd64.zip" | awk '{print $1}')"
  actual_arm64_sha="$(sha256sum "${TMP_DIR}/fleet_${version}_windows_arm64.zip" | awk '{print $1}')"
else
  actual_x64_sha="$(shasum -a 256 "${TMP_DIR}/fleet_${version}_windows_amd64.zip" | awk '{print $1}')"
  actual_arm64_sha="$(shasum -a 256 "${TMP_DIR}/fleet_${version}_windows_arm64.zip" | awk '{print $1}')"
fi
"${ROOT_DIR}/scripts/generate-winget-manifests.sh" \
  "${version}" "${x64_url}" "${actual_x64_sha}" "${arm64_url}" "${actual_arm64_sha}" \
  "${TMP_DIR}/bound-manifests" 2026-09-05 >/dev/null
"${ROOT_DIR}/scripts/validate-winget-manifests.sh" "${TMP_DIR}/bound-manifests" \
  "${TMP_DIR}/fleet_${version}_windows_amd64.zip" "${TMP_DIR}/fleet_${version}_windows_arm64.zip" >/dev/null

if "${ROOT_DIR}/scripts/generate-winget-manifests.sh" \
  "${version}" "http://example.invalid/fleet.zip" "${x64_sha}" \
  "${arm64_url}" "${arm64_sha}" "${TMP_DIR}/bad-url" >/dev/null 2>&1; then
  echo "WinGet generator accepted a non-release x64 URL" >&2
  exit 1
fi

if "${ROOT_DIR}/scripts/generate-winget-manifests.sh" \
  "${version}" "${x64_url}" deadbeef \
  "${arm64_url}" "${arm64_sha}" "${TMP_DIR}/bad-hash" >/dev/null 2>&1; then
  echo "WinGet generator accepted a malformed SHA-256" >&2
  exit 1
fi

echo "WinGet manifest generation checks passed"
