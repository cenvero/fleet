#!/bin/sh
set -eu

BASE_URL="https://fleet.cenvero.org"
CHANNEL="${FLEET_CHANNEL:-stable}"
FLEET_VERSION="${FLEET_VERSION:-}"
MINISIGN_PUBKEY='RWRb53p9WTsWCO2RZT3bvjrZw4QjXnIo2R7NUqhPsfvhR8u0sS55hZb3'

# ── colours ────────────────────────────────────────────────────────────────
if [ -t 1 ]; then
  BOLD='\033[1m'; DIM='\033[2m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'
  YELLOW='\033[0;33m'; RED='\033[0;31m'; RESET='\033[0m'
else
  BOLD=''; DIM=''; GREEN=''; CYAN=''; YELLOW=''; RED=''; RESET=''
fi

step()  { printf "${BOLD}${CYAN}  ==> ${RESET}${BOLD}%s${RESET}\n" "$1"; }
ok()    { printf "${GREEN}   ✓  ${RESET}%s\n" "$1"; }
warn()  { printf "${YELLOW}   !  ${RESET}%s\n" "$1"; }
die()   { printf "${RED}  ✗  %s${RESET}\n" "$1" >&2; exit 1; }
ask()   { printf "${BOLD}%s${RESET} [y/N]: " "$1"; }

# ── header ─────────────────────────────────────────────────────────────────
printf "\n${BOLD}  Cenvero Fleet installer${RESET}${DIM}  fleet.cenvero.org${RESET}\n"
printf "${DIM}  ─────────────────────────────────────────────────${RESET}\n\n"

# ── detect platform (needed for pkg manager logic below) ───────────────────
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)    ARCH="amd64"  ;;
  aarch64|arm64)   ARCH="arm64"  ;;
  armv7l)          ARCH="armv7"  ;;
  *) die "unsupported architecture: $ARCH" ;;
esac
TARGET="${OS}-${ARCH}"

# ── hard requirements: curl and tar ────────────────────────────────────────
for dep in curl tar; do
  command -v "$dep" >/dev/null 2>&1 || die "missing required tool: $dep — install it and re-run"
done

# ── pkg_install helper — used for both jq and minisign ─────────────────────
# Usage: pkg_install <package>
pkg_install() {
  _pkg="$1"
  if [ "$OS" = "darwin" ]; then
    if ! command -v brew >/dev/null 2>&1; then
      warn "Homebrew is not installed. Visit https://brew.sh"
      return 1
    fi
    brew install "${_pkg}"
  elif [ "$OS" = "linux" ]; then
    if command -v apt-get >/dev/null 2>&1; then
      sudo apt-get install -y "${_pkg}"
    elif command -v dnf >/dev/null 2>&1; then
      sudo dnf install -y "${_pkg}"
    elif command -v yum >/dev/null 2>&1; then
      sudo yum install -y "${_pkg}"
    elif command -v pacman >/dev/null 2>&1; then
      sudo pacman -S --noconfirm "${_pkg}"
    elif command -v apk >/dev/null 2>&1; then
      sudo apk add "${_pkg}"
    else
      warn "Could not detect a package manager. Install ${_pkg} manually and re-run."
      return 1
    fi
  fi
}

# ── software check ─────────────────────────────────────────────────────────
# Tell the user upfront what may be installed, then handle each dep.

NEED_JQ=0
NEED_MINISIGN=0
command -v jq        >/dev/null 2>&1 || NEED_JQ=1
command -v minisign  >/dev/null 2>&1 || NEED_MINISIGN=1

if [ "${NEED_JQ}" = "1" ] || [ "${NEED_MINISIGN}" = "1" ]; then
  printf "${BOLD}  The installer needs the following software:${RESET}\n\n"
  [ "${NEED_JQ}"       = "1" ] && printf "    • jq        ${RED}(required)${RESET}  — parse the release manifest\n"
  [ "${NEED_MINISIGN}" = "1" ] && printf "    • minisign  ${YELLOW}(recommended)${RESET} — verify release signatures\n"
  printf "\n"
  printf "  ${DIM}It will use %s to install missing packages.${RESET}\n" \
    "$([ "$OS" = "darwin" ] && echo "Homebrew" || echo "your package manager (with sudo)")"
  printf "\n"
fi

# jq — required, cannot skip
if [ "${NEED_JQ}" = "1" ]; then
  ask "Install jq now? (required — cannot continue without it)"
  read -r REPLY </dev/tty || REPLY="n"
  case "$REPLY" in
    [Yy]|[Yy][Ee][Ss])
      step "Installing jq"
      pkg_install jq || die "Failed to install jq. Install it manually and re-run."
      ok "jq installed"
      ;;
    *)
      die "jq is required. Install it and re-run the installer."
      ;;
  esac
  printf "\n"
fi

# minisign — optional, but strongly recommended
HAS_MINISIGN=1
command -v minisign >/dev/null 2>&1 || HAS_MINISIGN=0

if [ "${NEED_MINISIGN}" = "1" ]; then
  ask "Install minisign now? (recommended for signature verification)"
  read -r REPLY </dev/tty || REPLY="n"
  case "$REPLY" in
    [Yy]|[Yy][Ee][Ss])
      step "Installing minisign"
      if pkg_install minisign; then
        HAS_MINISIGN=1
        ok "minisign installed"
      else
        warn "Could not install minisign — continuing without signature verification."
      fi
      ;;
    *)
      warn "Skipping minisign — release signature will not be verified."
      warn "Checksum (sha256) will still be verified."
      ;;
  esac
  printf "\n"
fi

# ── fetch manifest ─────────────────────────────────────────────────────────
step "Fetching release manifest"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

MANIFEST_PATH="${TMP_DIR}/manifest.json"
curl -fsSL "${BASE_URL}/manifest.json" -o "${MANIFEST_PATH}"

if [ -n "${FLEET_VERSION}" ]; then
  VERSION="${FLEET_VERSION}"
else
  VERSION="$(jq -r --arg c "$CHANNEL" '.channels[$c].version // empty' "${MANIFEST_PATH}")"
  [ -n "${VERSION}" ] || die "channel not found: ${CHANNEL}"
fi
ok "Channel: ${CHANNEL}  →  ${VERSION}"

# ── resolve download URL ───────────────────────────────────────────────────
URL="$(jq -r --arg v "$VERSION" --arg t "$TARGET" '.binaries[$v][$t].url // empty' "${MANIFEST_PATH}")"
SIG_URL="$(jq -r --arg v "$VERSION" --arg t "$TARGET" '.binaries[$v][$t].signature_url // empty' "${MANIFEST_PATH}")"
SHA="$(jq -r --arg v "$VERSION" --arg t "$TARGET" '.binaries[$v][$t].sha256 // empty' "${MANIFEST_PATH}")"

if [ -z "${URL}" ]; then
  if [ -n "${FLEET_VERSION}" ]; then
    URL="https://github.com/cenvero/fleet/releases/download/${VERSION}/fleet_${VERSION}_${OS}_${ARCH}.tar.gz"
    SIG_URL="${URL}.minisig"
    SHA=""
  else
    die "no release for ${TARGET} — check https://github.com/cenvero/fleet/releases"
  fi
fi

# ── download ───────────────────────────────────────────────────────────────
step "Downloading fleet ${VERSION} for ${TARGET}"
ARCHIVE_PATH="${TMP_DIR}/fleet.tar.gz"
curl -fsSL --progress-bar "${URL}" -o "${ARCHIVE_PATH}"
ok "Download complete"

# ── verify signature ───────────────────────────────────────────────────────
if [ -n "${SIG_URL}" ] && [ "${SIG_URL}" != "null" ]; then
  curl -fsSL "${SIG_URL}" -o "${TMP_DIR}/fleet.minisig" 2>/dev/null || true
  if [ "${HAS_MINISIGN}" = "1" ] && [ -f "${TMP_DIR}/fleet.minisig" ]; then
    step "Verifying signature"
    [ "${MINISIGN_PUBKEY}" != "REPLACE_WITH_MINISIGN_PUBLIC_KEY" ] || \
      die "installer public key placeholder has not been replaced"
    minisign -Vm "${ARCHIVE_PATH}" -P "${MINISIGN_PUBKEY}" -x "${TMP_DIR}/fleet.minisig" >/dev/null 2>&1 \
      && ok "Signature verified" \
      || die "Signature verification failed — aborting"
  fi
fi

# ── verify checksum ────────────────────────────────────────────────────────
if [ -n "${SHA}" ] && [ "${SHA}" != "null" ]; then
  step "Verifying checksum"
  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "${ARCHIVE_PATH}" | awk '{print $1}')"
  else
    ACTUAL="$(shasum -a 256 "${ARCHIVE_PATH}" | awk '{print $1}')"
  fi
  [ "${ACTUAL}" = "${SHA}" ] || die "checksum mismatch — download may be corrupt"
  ok "Checksum verified"
fi

# ── extract ────────────────────────────────────────────────────────────────
tar -xzf "${ARCHIVE_PATH}" -C "${TMP_DIR}"
SOURCE_FILE="$(find "${TMP_DIR}" -type f \( -name 'fleet' -o -name 'fleet.exe' \) | head -n 1)"
[ -n "${SOURCE_FILE}" ] || die "fleet binary not found in archive"

# ── install ────────────────────────────────────────────────────────────────
step "Installing fleet"

INSTALL_DIR="/usr/local/bin"
USED_SUDO=0

if [ "$OS" = "linux" ]; then
  # On Linux always install to /usr/local/bin — use sudo
  if [ "$(id -u)" = "0" ]; then
    install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
  else
    printf "${DIM}   (sudo required to install to %s)${RESET}\n" "${INSTALL_DIR}"
    sudo install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
    USED_SUDO=1
  fi

elif [ "$OS" = "darwin" ]; then
  # On macOS prefer /usr/local/bin (writable after brew setup) or /opt/homebrew/bin
  if [ -w "${INSTALL_DIR}" ]; then
    install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
  elif [ -d "/opt/homebrew/bin" ] && [ -w "/opt/homebrew/bin" ]; then
    INSTALL_DIR="/opt/homebrew/bin"
    install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
  else
    printf "${DIM}   (sudo required to install to %s)${RESET}\n" "${INSTALL_DIR}"
    sudo install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
    USED_SUDO=1
  fi

else
  # Fallback
  if [ -w "${INSTALL_DIR}" ]; then
    install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
  else
    INSTALL_DIR="${HOME}/.local/bin"
    mkdir -p "${INSTALL_DIR}"
    install -m 0755 "${SOURCE_FILE}" "${INSTALL_DIR}/fleet"
  fi
fi

ok "Installed to ${INSTALL_DIR}/fleet"

# ── PATH check ─────────────────────────────────────────────────────────────
IN_PATH=0
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) IN_PATH=1 ;;
esac

if [ "${IN_PATH}" = "0" ]; then
  printf "\n"
  warn "${INSTALL_DIR} is not in your PATH."
  if [ "$OS" = "linux" ]; then
    warn "Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    printf "    ${DIM}export PATH=\"${INSTALL_DIR}:\$PATH\"${RESET}\n"
  elif [ "$OS" = "darwin" ]; then
    warn "Add this to ~/.zshrc (or ~/.bash_profile):"
    printf "    ${DIM}export PATH=\"${INSTALL_DIR}:\$PATH\"${RESET}\n"
  fi
fi

# ── done ───────────────────────────────────────────────────────────────────
printf "\n${BOLD}${GREEN}  ✓  Cenvero Fleet ${VERSION} installed successfully!${RESET}\n\n"
printf "  Get started:\n\n"
printf "    ${BOLD}fleet init${RESET}             ${DIM}# set up controller config${RESET}\n"
printf "    ${BOLD}fleet server add${RESET}       ${DIM}# add your first server (interactive)${RESET}\n"
printf "    ${BOLD}fleet dashboard${RESET}        ${DIM}# open the terminal dashboard${RESET}\n\n"
printf "  Docs:  ${CYAN}https://fleet.cenvero.org/docs/${RESET}\n\n"
