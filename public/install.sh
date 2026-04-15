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

# ── required deps ──────────────────────────────────────────────────────────
for dep in curl tar jq; do
  command -v "$dep" >/dev/null 2>&1 || die "missing required dependency: $dep — install it and re-run"
done

# ── detect platform ────────────────────────────────────────────────────────
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)    ARCH="amd64"  ;;
  aarch64|arm64)   ARCH="arm64"  ;;
  armv7l)          ARCH="armv7"  ;;
  *) die "unsupported architecture: $ARCH" ;;
esac
TARGET="${OS}-${ARCH}"

# ── minisign check ─────────────────────────────────────────────────────────
HAS_MINISIGN=1
command -v minisign >/dev/null 2>&1 || HAS_MINISIGN=0

if [ "${HAS_MINISIGN}" = "0" ]; then
  printf "\n"
  warn "minisign is not installed — it is used to verify release signatures."
  warn "We ${BOLD}strongly recommend${RESET} installing it before continuing."
  printf "\n"

  if [ "$OS" = "darwin" ]; then
    # macOS — install via Homebrew, no sudo needed
    if ! command -v brew >/dev/null 2>&1; then
      warn "Homebrew is not installed either. Visit https://brew.sh to install it first."
      ask "Continue without signature verification?"
      read -r REPLY </dev/tty || REPLY="n"
      case "$REPLY" in
        [Yy]|[Yy][Ee][Ss]) warn "Continuing — checksum will still be verified." ;;
        *) die "Aborted." ;;
      esac
    else
      ask "Install minisign via Homebrew now and continue?"
      read -r REPLY </dev/tty || REPLY="n"
      case "$REPLY" in
        [Yy]|[Yy][Ee][Ss])
          step "Installing minisign via Homebrew"
          brew install minisign
          HAS_MINISIGN=1
          ok "minisign installed"
          ;;
        *)
          warn "Continuing without signature verification."
          ;;
      esac
    fi

  elif [ "$OS" = "linux" ]; then
    # Linux — detect package manager and install with sudo
    if command -v apt-get >/dev/null 2>&1; then
      PKG_CMD="sudo apt-get install -y minisign"
    elif command -v dnf >/dev/null 2>&1; then
      PKG_CMD="sudo dnf install -y minisign"
    elif command -v yum >/dev/null 2>&1; then
      PKG_CMD="sudo yum install -y minisign"
    elif command -v pacman >/dev/null 2>&1; then
      PKG_CMD="sudo pacman -S --noconfirm minisign"
    elif command -v apk >/dev/null 2>&1; then
      PKG_CMD="sudo apk add minisign"
    else
      PKG_CMD=""
    fi

    if [ -n "${PKG_CMD}" ]; then
      ask "Install minisign now (${PKG_CMD}) and continue?"
      read -r REPLY </dev/tty || REPLY="n"
      case "$REPLY" in
        [Yy]|[Yy][Ee][Ss])
          step "Installing minisign"
          eval "${PKG_CMD}"
          HAS_MINISIGN=1
          ok "minisign installed"
          ;;
        *)
          warn "Continuing without signature verification."
          ;;
      esac
    else
      warn "Could not detect a package manager to install minisign."
      ask "Continue without signature verification?"
      read -r REPLY </dev/tty || REPLY="n"
      case "$REPLY" in
        [Yy]|[Yy][Ee][Ss]) warn "Continuing — checksum will still be verified." ;;
        *) die "Aborted. Install minisign manually and re-run." ;;
      esac
    fi
  fi
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
