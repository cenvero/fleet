#!/bin/sh
set -eu

removed=0
for target in "/usr/bin/fleet" "/usr/local/bin/fleet" "/opt/homebrew/bin/fleet" "${HOME}/.local/bin/fleet" "${HOME}/.local/bin/fleet.exe"; do
  [ -f "$target" ] || continue
  if [ -w "$target" ] || [ "$(id -u)" = "0" ]; then
    rm -f -- "$target"
  elif command -v sudo >/dev/null 2>&1; then
    sudo rm -f -- "$target"
  else
    echo "cannot remove $target without root privileges (sudo is unavailable)" >&2
    exit 1
  fi
  echo "removed $target"
  removed=1
done

[ "$removed" -eq 1 ] || echo "No Cenvero Fleet binary found in a standard install path."
echo "Cenvero Fleet binaries removed. Configuration directories are left intact."
