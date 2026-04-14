#!/bin/sh
set -eu

for target in "/usr/local/bin/fleet" "${HOME}/.local/bin/fleet" "${HOME}/.local/bin/fleet.exe"; do
  if [ -f "${target}" ]; then
    rm -f "${target}"
    echo "removed ${target}"
  fi
done

echo "Cenvero Fleet binaries removed. Configuration directories are left intact."
