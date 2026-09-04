#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: validate-winget-manifests.sh <manifest-dir> [<x64-zip> <arm64-zip>]" >&2
  exit 2
}

[ "$#" -eq 1 ] || [ "$#" -eq 3 ] || usage
manifest_dir="$1"
version_file="${manifest_dir}/Cenvero.Fleet.yaml"
locale_file="${manifest_dir}/Cenvero.Fleet.locale.en-US.yaml"
installer_file="${manifest_dir}/Cenvero.Fleet.installer.yaml"

for file in "${version_file}" "${locale_file}" "${installer_file}"; do
  [ -s "${file}" ] || { echo "missing WinGet manifest: ${file}" >&2; exit 1; }
  grep -Fx 'PackageIdentifier: Cenvero.Fleet' "${file}" >/dev/null
  grep -Fx 'ManifestVersion: 1.12.0' "${file}" >/dev/null
  if grep -Eq '[[:space:]]+$' "${file}"; then
    echo "trailing whitespace in ${file}" >&2
    exit 1
  fi
done

grep -Fx '# yaml-language-server: $schema=https://aka.ms/winget-manifest.version.1.12.0.schema.json' "${version_file}" >/dev/null
grep -Fx '# yaml-language-server: $schema=https://aka.ms/winget-manifest.defaultLocale.1.12.0.schema.json' "${locale_file}" >/dev/null
grep -Fx '# yaml-language-server: $schema=https://aka.ms/winget-manifest.installer.1.12.0.schema.json' "${installer_file}" >/dev/null

version="$(awk '/^PackageVersion: / {print $2; exit}' "${version_file}")"
[[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || { echo "invalid generated package version: ${version}" >&2; exit 1; }
for file in "${locale_file}" "${installer_file}"; do
  [ "$(awk '/^PackageVersion: / {print $2; exit}' "${file}")" = "${version}" ] || {
    echo "PackageVersion mismatch in ${file}" >&2
    exit 1
  }
done

[ "$(grep -Fc '  - Architecture: x64' "${installer_file}")" -eq 1 ]
[ "$(grep -Fc '  - Architecture: arm64' "${installer_file}")" -eq 1 ]
[ "$(grep -Fc '    InstallerType: zip' "${installer_file}")" -eq 2 ]
[ "$(grep -Fc '    NestedInstallerType: portable' "${installer_file}")" -eq 2 ]
[ "$(grep -Fc '      - RelativeFilePath: fleet.exe' "${installer_file}")" -eq 2 ]
[ "$(grep -Fc '        PortableCommandAlias: fleet' "${installer_file}")" -eq 2 ]
[ "$(grep -Ec '^    InstallerSha256: [0-9A-F]{64}$' "${installer_file}")" -eq 2 ]

grep -Fx "    InstallerUrl: https://github.com/cenvero/fleet/releases/download/v${version}/fleet_${version}_windows_amd64.zip" "${installer_file}" >/dev/null
grep -Fx "    InstallerUrl: https://github.com/cenvero/fleet/releases/download/v${version}/fleet_${version}_windows_arm64.zip" "${installer_file}" >/dev/null

if command -v ruby >/dev/null 2>&1; then
  ruby -e 'require "yaml"; ARGV.each { |f| Psych.parse_file(f) or raise "empty YAML document: #{f}" }' "${version_file}" "${locale_file}" "${installer_file}"
fi

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print toupper($1)}'
  else shasum -a 256 "$1" | awk '{print toupper($1)}'
  fi
}

if [ "$#" -eq 3 ]; then
  x64_zip="$2"
  arm64_zip="$3"
  command -v unzip >/dev/null 2>&1 || { echo "unzip is required" >&2; exit 1; }
  for archive in "${x64_zip}" "${arm64_zip}"; do
    [ -s "${archive}" ] || { echo "missing release archive: ${archive}" >&2; exit 1; }
    members="$(unzip -Z1 "${archive}" | sed '/\/$/d')"
    [ "$(printf '%s\n' "${members}" | grep -Fxc 'fleet.exe')" -eq 1 ] || {
      echo "WinGet archive must contain one root fleet.exe: ${archive}" >&2
      printf '%s\n' "${members}" >&2
      exit 1
    }
    executables="$(printf '%s\n' "${members}" | grep -Ei '(^|/)[^/]+\.exe$' || true)"
    [ "${executables}" = "fleet.exe" ] || {
      echo "WinGet archive contains an unexpected executable: ${archive}" >&2
      printf '%s\n' "${executables}" >&2
      exit 1
    }
    if printf '%s\n' "${members}" | grep -Eq '(^/|(^|/)\.\.(/|$)|\\)'; then
      echo "WinGet archive contains an unsafe member path: ${archive}" >&2
      exit 1
    fi
  done
  x64_declared="$(awk '/^    InstallerSha256: / {print $2}' "${installer_file}" | sed -n '1p')"
  arm64_declared="$(awk '/^    InstallerSha256: / {print $2}' "${installer_file}" | sed -n '2p')"
  [ "${x64_declared}" = "$(sha256_file "${x64_zip}")" ] || { echo "x64 manifest hash mismatch" >&2; exit 1; }
  [ "${arm64_declared}" = "$(sha256_file "${arm64_zip}")" ] || { echo "arm64 manifest hash mismatch" >&2; exit 1; }
fi

echo "WinGet manifest bundle validation passed: Cenvero.Fleet ${version}"
