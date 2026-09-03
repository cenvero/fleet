#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT INT TERM
mkdir -p "${TMP_DIR}/public" "${TMP_DIR}/internal/update" "${TMP_DIR}/dist" "${TMP_DIR}/bin"
cp "${ROOT_DIR}/public/install" "${ROOT_DIR}/public/install.sh" "${ROOT_DIR}/public/install.ps1" "${TMP_DIR}/public/"

cat >"${TMP_DIR}/public/manifest.json" <<'JSON'
{"generated_at":"2026-04-13T00:00:00Z","channels":{"stable":{"version":"v0.1.0","history":["v0.1.0"]},"beta":{"version":"v0.1.0","history":["v0.1.0"]},"alpha":{"version":"v0.1.0","history":["v0.1.0"]}},"binaries":{},"agent_binaries":{}}
JSON
printf 'untrusted comment: test\nREPLACE_WITH_MINISIGN_PUBLIC_KEY\n' >"${TMP_DIR}/public/signing.pub"
cp "${TMP_DIR}/public/signing.pub" "${TMP_DIR}/internal/update/signing.pub"

FLEET_ROOT_DIR="${TMP_DIR}" FLEET_MINISIGN_PUBLIC_KEY=$'untrusted comment: test\nABC123PUBLICKEYPAYLOAD' "${ROOT_DIR}/scripts/sync-signing-key.sh"
FLEET_ROOT_DIR="${TMP_DIR}" FLEET_MINISIGN_PUBLIC_KEY=$'untrusted comment: test\nABC123PUBLICKEYPAYLOAD' "${ROOT_DIR}/scripts/validate-signing-assets.sh"
grep -q ABC123PUBLICKEYPAYLOAD "${TMP_DIR}/public/install.sh"
grep -q ABC123PUBLICKEYPAYLOAD "${TMP_DIR}/public/install.ps1"

MINISIGN_SECRET_KEY="$(printf 'test-secret'|base64)" MINISIGN_PASSWORD=test MINISIGN_PUBLIC_KEY=ABC123PUBLICKEYPAYLOAD "${ROOT_DIR}/scripts/validate-release-env.sh"

artifacts='[]'
for binary in fleet fleet-agent; do
  for spec in darwin:amd64: darwin:arm64: linux:amd64: linux:arm64: linux:arm:7 windows:amd64: windows:arm64:; do
    IFS=: read -r goos goarch goarm <<<"${spec}"
    target_arch="${goarch}"; [ "${goarch}" = arm ] && target_arch=armv7
    ext=tar.gz; type=Archive; [ "${goos}" = windows ] && { ext=zip; type=Zip; }
    name="${binary}_1.2.3_${goos}_${target_arch}.${ext}"
    path="${TMP_DIR}/dist/${name}"
    printf '%s\n' "${binary}-${goos}-${target_arch}" >"${path}"
    if command -v sha256sum >/dev/null 2>&1; then hash="$(sha256sum "${path}"|awk '{print $1}')"; else hash="$(shasum -a 256 "${path}"|awk '{print $1}')"; fi
    cat >"${path}.minisig" <<SIG
untrusted comment: test signature
AAAA
trusted comment: cenvero-fleet ${binary} v1.2.3 ${goos}-${target_arch}
BBBB
SIG
    artifacts="$(jq -cn --argjson a "${artifacts}" --arg name "${name}" --arg path "${path}" --arg os "${goos}" --arg arch "${goarch}" --arg arm "${goarm}" --arg type "${type}" --arg sum "sha256:${hash}" '$a+[{name:$name,path:$path,goos:$os,goarch:$arch,goarm:$arm,type:$type,extra:{Binary:(if $name|startswith("fleet-agent_") then "fleet-agent" else "fleet" end),Checksum:$sum}}]')"
  done
done
printf '%s\n' "${artifacts}" >"${TMP_DIR}/dist/artifacts.json"

# Missing signatures and incomplete target matrices must fail closed.
mv "${TMP_DIR}/dist/fleet_1.2.3_linux_amd64.tar.gz.minisig" "${TMP_DIR}/missing.minisig"
if FLEET_ROOT_DIR="${TMP_DIR}" FLEET_DIST_DIR="${TMP_DIR}/dist" FLEET_VERSION=v1.2.3 FLEET_CHANNEL=stable FLEET_RELEASE_DATE=2026-04-13T12:00:00Z FLEET_RELEASE_NOTES_URL=https://github.com/cenvero/fleet/releases/tag/v1.2.3 FLEET_REPOSITORY=cenvero/fleet "${ROOT_DIR}/scripts/update-manifest.sh" >/dev/null 2>&1; then
  echo "update-manifest accepted an unsigned artifact" >&2; exit 1
fi
mv "${TMP_DIR}/missing.minisig" "${TMP_DIR}/dist/fleet_1.2.3_linux_amd64.tar.gz.minisig"
cp "${TMP_DIR}/dist/artifacts.json" "${TMP_DIR}/artifacts.complete.json"
jq '.[0:-1]' "${TMP_DIR}/artifacts.complete.json" >"${TMP_DIR}/dist/artifacts.json"
if FLEET_ROOT_DIR="${TMP_DIR}" FLEET_DIST_DIR="${TMP_DIR}/dist" FLEET_VERSION=v1.2.3 FLEET_CHANNEL=stable FLEET_RELEASE_DATE=2026-04-13T12:00:00Z FLEET_RELEASE_NOTES_URL=https://github.com/cenvero/fleet/releases/tag/v1.2.3 FLEET_REPOSITORY=cenvero/fleet "${ROOT_DIR}/scripts/update-manifest.sh" >/dev/null 2>&1; then
  echo "update-manifest accepted an incomplete matrix" >&2; exit 1
fi
cp "${TMP_DIR}/artifacts.complete.json" "${TMP_DIR}/dist/artifacts.json"

FLEET_ROOT_DIR="${TMP_DIR}" FLEET_DIST_DIR="${TMP_DIR}/dist" FLEET_VERSION=v1.2.3 FLEET_CHANNEL=stable FLEET_RELEASE_DATE=2026-04-13T12:00:00Z FLEET_RELEASE_NOTES_URL=https://github.com/cenvero/fleet/releases/tag/v1.2.3 FLEET_REPOSITORY=cenvero/fleet "${ROOT_DIR}/scripts/update-manifest.sh"
FLEET_ROOT_DIR="${TMP_DIR}" FLEET_DIST_DIR="${TMP_DIR}/dist" FLEET_VERSION=v1.2.3 FLEET_REPOSITORY=cenvero/fleet "${ROOT_DIR}/scripts/validate-release-manifest.sh"
[ "$(jq '.binaries["v1.2.3"]|length' "${TMP_DIR}/public/manifest.json")" -eq 7 ]
[ "$(jq '.agent_binaries["v1.2.3"]|length' "${TMP_DIR}/public/manifest.json")" -eq 7 ]
jq -e '.binaries["v1.2.3"]["linux-armv7"].signature_url|endswith(".minisig")' "${TMP_DIR}/public/manifest.json" >/dev/null

# The signer must derive, rather than accept, the authenticated release binding.
cat >"${TMP_DIR}/bin/minisign" <<'SH'
#!/bin/sh
printf '%s\n' "$@" >"${SIGN_ARGS}"
SH
chmod +x "${TMP_DIR}/bin/minisign"
printf secret >"${TMP_DIR}/secret.key"
chmod 0600 "${TMP_DIR}/secret.key"
SIGN_ARGS="${TMP_DIR}/sign.args" PATH="${TMP_DIR}/bin:${PATH}" "${ROOT_DIR}/scripts/sign-release-artifact.sh" "${TMP_DIR}/dist/fleet_1.2.3_linux_amd64.tar.gz" "${TMP_DIR}/out.minisig" "${TMP_DIR}/secret.key"
grep -Fx 'cenvero-fleet fleet v1.2.3 linux-amd64' "${TMP_DIR}/sign.args" >/dev/null
if SIGN_ARGS="${TMP_DIR}/bad.args" PATH="${TMP_DIR}/bin:${PATH}" "${ROOT_DIR}/scripts/sign-release-artifact.sh" "${TMP_DIR}/dist/not-a-release.tar.gz" "${TMP_DIR}/bad.minisig" "${TMP_DIR}/secret.key" >/dev/null 2>&1; then
  echo "signer accepted an unbound artifact name" >&2; exit 1
fi

sh -n "${TMP_DIR}/public/install" "${TMP_DIR}/public/install.sh"
grep -q 'signature URL is required' "${TMP_DIR}/public/install.sh"
grep -q 'verification cannot be skipped' "${TMP_DIR}/public/install.sh"
grep -q 'EXPECTED_COMMENT="cenvero-fleet fleet' "${TMP_DIR}/public/install.sh"
grep -q 'Release signature URL is required' "${TMP_DIR}/public/install.ps1"
grep -q 'Signature is not bound' "${TMP_DIR}/public/install.ps1"
grep -Fq 'Add-Type -AssemblyName System.Net.Http' "${TMP_DIR}/public/install.ps1"
grep -Fq 'minisign-0.12-win64.zip' "${TMP_DIR}/public/install.ps1"
grep -Fq '37b600344e20c19314b2e82813db2bfdcc408b77b876f7727889dbd46d539479' "${TMP_DIR}/public/install.ps1"
if grep -Fq 'minisign is required' "${TMP_DIR}/public/install.ps1"; then
  echo "Windows installer still requires a preinstalled minisign executable" >&2; exit 1
fi
grep -Fq '[Environment]::SetEnvironmentVariable("PATH"' "${TMP_DIR}/public/install.ps1"
grep -Fq 'Added $installDir to your user PATH.' "${TMP_DIR}/public/install.ps1"
grep -Fq 'Add this directory to your user PATH manually:' "${TMP_DIR}/public/install.ps1"
if grep -q 'Read-Host\|FLEET_SKIP_PATH_UPDATE\|Skipped PATH update' "${TMP_DIR}/public/install.ps1"; then
  echo "Windows installer contains interactive or skip-based PATH behavior" >&2; exit 1
fi
grep -q -- '--max-redirs 0' "${TMP_DIR}/public/install" "${TMP_DIR}/public/install.sh"
grep -Fq "AllowAutoRedirect = \$false" "${TMP_DIR}/public/install.ps1"
grep -q 'GetHostAddresses' "${TMP_DIR}/public/install.ps1"
if grep -q 'Invoke-WebRequest\|-fsSL' "${TMP_DIR}/public/install" "${TMP_DIR}/public/install.sh" "${TMP_DIR}/public/install.ps1"; then
  echo "installer contains an automatically redirecting downloader" >&2; exit 1
fi
if grep -q 'continuing without signature\|signature verification skipped' "${TMP_DIR}/public/install.sh" "${TMP_DIR}/public/install.ps1"; then
  echo "installer contains a signature-verification bypass" >&2; exit 1
fi

# Execute the installer against a mocked manifest: a missing signature URL must
# abort before any archive is downloaded or installed.
mkdir -p "${TMP_DIR}/installer-bin"
cat >"${TMP_DIR}/installer-bin/uname" <<'SH'
#!/bin/sh
[ "${1:-}" = -s ] && { echo Linux; exit; }
[ "${1:-}" = -m ] && { echo x86_64; exit; }
exec /usr/bin/uname "$@"
SH
cat >"${TMP_DIR}/installer-bin/minisign" <<'SH'
#!/bin/sh
exit 99
SH
cat >"${TMP_DIR}/installer-bin/getent" <<'SH'
#!/bin/sh
[ "${1:-}" = ahosts ] || exit 1
printf '93.184.216.34 STREAM %s\n' "${2:-fleet.cenvero.org}"
SH
cat >"${TMP_DIR}/installer-bin/curl" <<'SH'
#!/bin/sh
out=
headers=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -D) headers="$2"; shift 2 ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
: >"$headers"
case "$url" in
  https://fleet.cenvero.org/manifest.json)
    cat >"$out" <<'JSON'
{"channels":{"stable":{"version":"v1.2.3"}},"binaries":{"v1.2.3":{"linux-amd64":{"url":"https://github.com/cenvero/fleet/releases/download/v1.2.3/fleet_1.2.3_linux_amd64.tar.gz","signature_url":"","sha256":"1111111111111111111111111111111111111111111111111111111111111111","size":123}}}}
JSON
    printf '200' ;;
  *) echo "unexpected artifact download: $url" >&2; exit 90 ;;
esac
SH
chmod +x "${TMP_DIR}/installer-bin/"*
if PATH="${TMP_DIR}/installer-bin:${PATH}" FLEET_CHANNEL=stable sh "${TMP_DIR}/public/install.sh" >"${TMP_DIR}/installer.out" 2>&1; then
  echo "installer accepted a manifest without a signature URL" >&2; exit 1
fi
grep -q 'release signature URL is required' "${TMP_DIR}/installer.out"

# An approved redirect hostname resolving private must be rejected before curl is
# invoked for it (the forbidden endpoint receives zero requests).
cat >"${TMP_DIR}/installer-bin/getent" <<'SH'
#!/bin/sh
[ "${1:-}" = ahosts ] || exit 1
case "${2:-}" in
  fleet.cenvero.org) printf '93.184.216.34 STREAM fleet.cenvero.org\n' ;;
  release-assets.githubusercontent.com) printf '127.0.0.1 STREAM release-assets.githubusercontent.com\n' ;;
  *) exit 1 ;;
esac
SH
cat >"${TMP_DIR}/installer-bin/curl" <<'SH'
#!/bin/sh
out=
headers=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -D) headers="$2"; shift 2 ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
printf '%s\n' "$url" >>"${FLEET_CURL_LOG}"
: >"$out"
printf 'Location: https://release-assets.githubusercontent.com/metadata\r\n' >"$headers"
printf '302'
SH
chmod +x "${TMP_DIR}/installer-bin/curl"
: >"${TMP_DIR}/curl.log"
if FLEET_CURL_LOG="${TMP_DIR}/curl.log" PATH="${TMP_DIR}/installer-bin:${PATH}" FLEET_CHANNEL=stable sh "${TMP_DIR}/public/install.sh" >"${TMP_DIR}/redirect.out" 2>&1; then
  echo "installer followed a forbidden redirect" >&2; exit 1
fi
[ "$(wc -l <"${TMP_DIR}/curl.log" | tr -d '[:space:]')" -eq 1 ] || { echo "forbidden redirect endpoint received a request" >&2; exit 1; }
if grep -q 'release-assets.githubusercontent.com' "${TMP_DIR}/curl.log"; then
  echo "forbidden redirect endpoint received a curl request" >&2; exit 1
fi
grep -q 'forbidden address' "${TMP_DIR}/redirect.out"

echo "release tooling smoke test passed"
