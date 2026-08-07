#!/bin/sh
set -eu

BASE_URL="https://fleet.cenvero.org"
CHANNEL="${FLEET_CHANNEL:-stable}"
FLEET_VERSION="${FLEET_VERSION:-}"
MINISIGN_PUBKEY='RWRb53p9WTsWCO2RZT3bvjrZw4QjXnIo2R7NUqhPsfvhR8u0sS55hZb3'

if [ -t 1 ]; then
  BOLD='\033[1m'; DIM='\033[2m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'
  YELLOW='\033[0;33m'; RED='\033[0;31m'; RESET='\033[0m'
else
  BOLD=''; DIM=''; GREEN=''; CYAN=''; YELLOW=''; RED=''; RESET=''
fi

step() { printf "${BOLD}${CYAN}  ==> ${RESET}${BOLD}%s${RESET}\n" "$1"; }
ok()   { printf "${GREEN}   ✓  ${RESET}%s\n" "$1"; }
warn() { printf "${YELLOW}   !  ${RESET}%s\n" "$1"; }
die()  { printf "${RED}  ✗  %s${RESET}\n" "$1" >&2; exit 1; }
ask()  { printf "${BOLD}%s${RESET} [y/N]: " "$1"; }

approved_host() {
  case "$1" in
    fleet.cenvero.org|github.com|release-assets.githubusercontent.com|objects.githubusercontent.com) return 0 ;;
    *) return 1 ;;
  esac
}

forbidden_ip() {
  _ip="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  case "${_ip}" in
    ::|::1|fc*|fd*|fe8*|fe9*|fea*|feb*|ff*) return 0 ;;
    ::ffff:*) _ip="${_ip##*:}" ;;
    *:*) return 1 ;;
  esac
  awk -v ip="${_ip}" 'BEGIN {
    n=split(ip,a,"."); if(n!=4) exit 0;
    for(i=1;i<=4;i++) if(a[i]!~/^[0-9]+$/ || a[i]<0 || a[i]>255) exit 0;
    if(a[1]==0 || a[1]==10 || a[1]==127 || a[1]>=224 ||
       (a[1]==100 && a[2]>=64 && a[2]<=127) ||
       (a[1]==100 && a[2]==100 && a[3]==100 && a[4]==200) ||
       (a[1]==169 && a[2]==254) ||
       (a[1]==172 && a[2]>=16 && a[2]<=31) ||
       (a[1]==192 && a[2]==168) ||
       (a[1]==198 && (a[2]==18 || a[2]==19))) exit 0;
    exit 1
  }'
}

resolve_public_host() {
  _host="$1"
  if command -v getent >/dev/null 2>&1; then
    _addresses="$(getent ahosts "${_host}" 2>/dev/null | awk '{print $1}' | sort -u)"
  elif command -v dscacheutil >/dev/null 2>&1; then
    _addresses="$(dscacheutil -q host -a name "${_host}" 2>/dev/null | awk '/ip_address:/{print $2}' | sort -u)"
  elif command -v dig >/dev/null 2>&1; then
    _addresses="$( { dig +short A "${_host}"; dig +short AAAA "${_host}"; } 2>/dev/null | sort -u)"
  elif command -v host >/dev/null 2>&1; then
    _addresses="$(host "${_host}" 2>/dev/null | awk '/has (IPv6 )?address/{print $NF}' | sort -u)"
  else
    die "no DNS resolver tool available for secure download validation"
  fi
  [ -n "${_addresses}" ] || die "approved download host did not resolve: ${_host}"
  _chosen=""
  for _address in ${_addresses}; do
    forbidden_ip "${_address}" && die "download host ${_host} resolved to forbidden address ${_address}"
    [ -n "${_chosen}" ] || _chosen="${_address}"
  done
  printf '%s\n' "${_chosen}"
}

validate_download_url() {
  _candidate="$1"
  case "${_candidate}" in https://*) ;; *) die "refusing non-HTTPS download URL: ${_candidate}" ;; esac
  _authority="${_candidate#https://}"; _authority="${_authority%%/*}"
  case "${_authority}" in *'@'*|*':'*) die "refusing credentialed or non-default-port download URL: ${_candidate}" ;; esac
  _host="$(printf '%s' "${_authority}" | tr '[:upper:]' '[:lower:]')"
  approved_host "${_host}" || die "refusing unapproved download host: ${_host}"
  DOWNLOAD_HOST="${_host}"
  DOWNLOAD_IP="$(resolve_public_host "${_host}")" || die "download host address validation failed: ${_host}"
}

# Automatic redirects stay disabled. Every Location is validated and DNS-pinned
# before curl can contact the next hop, preventing redirect and DNS-rebinding SSRF.
download() {
  _url="$1"; _out="$2"; _hop=0
  _headers="${_out}.headers"; _part="${_out}.part"
  rm -f "${_headers}" "${_part}"
  while :; do
    validate_download_url "${_url}"
    case "${DOWNLOAD_IP}" in *:*) _resolve="${DOWNLOAD_HOST}:443:[${DOWNLOAD_IP}]" ;; *) _resolve="${DOWNLOAD_HOST}:443:${DOWNLOAD_IP}" ;; esac
    _status="$(curl -sS --noproxy '*' --proto '=https' --max-redirs 0 --resolve "${_resolve}" -D "${_headers}" -o "${_part}" -w '%{http_code}' "${_url}")" || {
      rm -f "${_headers}" "${_part}"; return 1;
    }
    case "${_status}" in
      200) mv "${_part}" "${_out}"; rm -f "${_headers}"; return 0 ;;
      301|302|303|307|308)
        [ "${_hop}" -lt 5 ] || { rm -f "${_headers}" "${_part}"; die "too many download redirects"; }
        _location="$(awk 'tolower(substr($0,1,9))=="location:"{sub(/^[^:]*:[[:space:]]*/,""); sub(/\r$/,""); value=$0} END{print value}' "${_headers}")"
        [ -n "${_location}" ] || { rm -f "${_headers}" "${_part}"; die "redirect missing Location header"; }
        case "${_location}" in https://*) _url="${_location}" ;; /*) _url="https://${DOWNLOAD_HOST}${_location}" ;; *) rm -f "${_headers}" "${_part}"; die "refusing ambiguous redirect Location: ${_location}" ;; esac
        _hop=$((_hop + 1)); rm -f "${_part}" ;;
      *) rm -f "${_headers}" "${_part}"; die "unexpected download HTTP status: ${_status}" ;;
    esac
  done
}

pkg_install() {
  _pkg="$1"
  if [ "$OS" = "darwin" ]; then
    command -v brew >/dev/null 2>&1 || return 1
    brew install "${_pkg}"
  elif [ "$OS" = "linux" ]; then
    if command -v apt-get >/dev/null 2>&1; then sudo apt-get install -y "${_pkg}"
    elif command -v dnf >/dev/null 2>&1; then sudo dnf install -y "${_pkg}"
    elif command -v yum >/dev/null 2>&1; then sudo yum install -y "${_pkg}"
    elif command -v pacman >/dev/null 2>&1; then sudo pacman -S --noconfirm "${_pkg}"
    elif command -v apk >/dev/null 2>&1; then sudo apk add "${_pkg}"
    else return 1
    fi
  else
    return 1
  fi
}

printf '\n%s  Cenvero Fleet installer%s%s  fleet.cenvero.org%s\n' "${BOLD}" "${RESET}" "${DIM}" "${RESET}"
printf '%s  ─────────────────────────────────────────────────%s\n\n' "${DIM}" "${RESET}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$OS" in linux|darwin) ;; *) die "unsupported operating system: $OS" ;; esac
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  armv7l) ARCH="armv7" ;;
  *) die "unsupported architecture: $ARCH" ;;
esac
TARGET="${OS}-${ARCH}"

for dep in curl tar; do
  command -v "$dep" >/dev/null 2>&1 || die "missing required tool: $dep — install it and re-run"
done

for dep in jq minisign; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    ask "Install ${dep} now? (required)"
    read -r REPLY </dev/tty || REPLY="n"
    case "$REPLY" in
      [Yy]|[Yy][Ee][Ss]) step "Installing ${dep}"; pkg_install "$dep" || die "Failed to install ${dep}. Install it manually and re-run." ;;
      *) die "${dep} is required; verification cannot be skipped." ;;
    esac
  fi
done
[ "${MINISIGN_PUBKEY}" != "REPLACE_WITH_MINISIGN_PUBLIC_KEY" ] || die "installer public key is not configured"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM HUP
MANIFEST_PATH="${TMP_DIR}/manifest.json"
step "Fetching release manifest"
download "${BASE_URL}/manifest.json" "${MANIFEST_PATH}" || die "failed to download release manifest"

if [ -n "${FLEET_VERSION}" ]; then VERSION="${FLEET_VERSION}"
else VERSION="$(jq -er --arg c "$CHANNEL" '.channels[$c].version | select(type == "string" and length > 0)' "${MANIFEST_PATH}")" || die "channel not found: ${CHANNEL}"
fi
printf '%s' "${VERSION}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$' || die "invalid release version: ${VERSION}"
VERSION_NO_V="${VERSION#v}"

URL="$(jq -er --arg v "$VERSION" --arg t "$TARGET" '.binaries[$v][$t].url | select(type == "string" and length > 0)' "${MANIFEST_PATH}")" || die "no release for ${VERSION} ${TARGET}"
SIG_URL="$(jq -er --arg v "$VERSION" --arg t "$TARGET" '.binaries[$v][$t].signature_url | select(type == "string" and length > 0)' "${MANIFEST_PATH}")" || die "release signature URL is required"
SHA="$(jq -er --arg v "$VERSION" --arg t "$TARGET" '.binaries[$v][$t].sha256 | select(type == "string" and test("^[0-9a-fA-F]{64}$"))' "${MANIFEST_PATH}")" || die "valid SHA-256 checksum is required"
SIZE="$(jq -er --arg v "$VERSION" --arg t "$TARGET" '.binaries[$v][$t].size | select(type == "number" and floor == . and . > 0 and . <= 1073741824)' "${MANIFEST_PATH}")" || die "valid artifact size is required"

EXT="tar.gz"
[ "$OS" = "windows" ] && EXT="zip"
EXPECTED_URL="https://github.com/cenvero/fleet/releases/download/${VERSION}/fleet_${VERSION_NO_V}_${OS}_${ARCH}.${EXT}"
[ "${URL}" = "${EXPECTED_URL}" ] || die "manifest URL does not match product, version, and target"
[ "${SIG_URL}" = "${URL}.minisig" ] || die "manifest signature URL does not match the archive URL"

step "Downloading fleet ${VERSION} for ${TARGET}"
ARCHIVE_PATH="${TMP_DIR}/fleet.${EXT}"
SIG_PATH="${TMP_DIR}/fleet.minisig"
download "${URL}" "${ARCHIVE_PATH}" || die "archive download failed"
ACTUAL_SIZE="$(wc -c <"${ARCHIVE_PATH}" | tr -d '[:space:]')"
[ "${ACTUAL_SIZE}" = "${SIZE}" ] || die "artifact size mismatch"
download "${SIG_URL}" "${SIG_PATH}" || die "signature download failed"

step "Verifying signature and release binding"
minisign -Vm "${ARCHIVE_PATH}" -P "${MINISIGN_PUBKEY}" -x "${SIG_PATH}" >/dev/null 2>&1 || die "signature verification failed"
TRUSTED_COMMENT="$(sed -n '3s/^trusted comment: //p' "${SIG_PATH}")"
EXPECTED_COMMENT="cenvero-fleet fleet ${VERSION} ${TARGET}"
[ "${TRUSTED_COMMENT}" = "${EXPECTED_COMMENT}" ] || die "signature is not bound to ${VERSION} ${TARGET}"
ok "Signature verified for ${VERSION} ${TARGET}"

step "Verifying checksum"
if command -v sha256sum >/dev/null 2>&1; then ACTUAL="$(sha256sum "${ARCHIVE_PATH}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then ACTUAL="$(shasum -a 256 "${ARCHIVE_PATH}" | awk '{print $1}')"
else die "sha256sum or shasum is required"
fi
[ "$(printf '%s' "$ACTUAL" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$SHA" | tr '[:upper:]' '[:lower:]')" ] || die "checksum mismatch"
ok "Checksum verified"

SOURCE_FILE="${TMP_DIR}/fleet"
MEMBER_COUNT="$(tar -tzf "${ARCHIVE_PATH}" | awk '$0=="fleet"{n++} END{print n+0}')"
[ "${MEMBER_COUNT}" -eq 1 ] || die "archive must contain exactly one canonical fleet binary"
tar -xOzf "${ARCHIVE_PATH}" fleet >"${SOURCE_FILE}"
[ -s "${SOURCE_FILE}" ] || die "fleet binary not found in archive"

step "Installing fleet"
INSTALL_DIR="/usr/local/bin"
if [ "$OS" = "linux" ]; then
  INSTALL_DIR="/usr/bin"
  if [ "$(id -u)" = "0" ]; then install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
  else sudo install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
  fi
elif [ -w "${INSTALL_DIR}" ]; then install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
elif [ -d "/opt/homebrew/bin" ] && [ -w "/opt/homebrew/bin" ]; then INSTALL_DIR="/opt/homebrew/bin"; install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
else sudo install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
fi
ok "Installed to ${INSTALL_DIR}/fleet"

case ":${PATH}:" in *":${INSTALL_DIR}:"*) ;; *) warn "${INSTALL_DIR} is not in PATH." ;; esac
printf '\n%s%s  ✓  Cenvero Fleet %s installed successfully!%s\n\n' "${BOLD}" "${GREEN}" "${VERSION}" "${RESET}"
printf '  Run: %sfleet init%s\n  Docs: %shttps://fleet.cenvero.org/docs/%s\n\n' "${BOLD}" "${RESET}" "${CYAN}" "${RESET}"
