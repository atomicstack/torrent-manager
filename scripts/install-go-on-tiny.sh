#!/usr/bin/env bash
# install-go-on-tiny.sh — install go1.26.3 into $HOME on a seedhost shell account
# that has no root and no package manager access.
#
# does, in order:
#   1. fetch go1.26.3.linux-amd64.tar.gz to $HOME/dl/ (skip if already present)
#   2. unpack to $HOME/go1.26.3/
#   3. symlink $HOME/go → $HOME/go1.26.3 (idempotent)
#   4. symlink every binary in $HOME/go/bin/ to $HOME/bin/ (idempotent)
#
# idempotent: safe to re-run. it does not alter shell rc files; the caller is
# expected to already have $HOME/bin on $PATH.
set -euo pipefail

GO_VERSION="${GO_VERSION:-go1.26.3}"
ARCH="${ARCH:-linux-amd64}"
TARBALL="${GO_VERSION}.${ARCH}.tar.gz"
URL="https://go.dev/dl/${TARBALL}"

HOME_BIN="${HOME}/bin"
DL_DIR="${HOME}/dl"
TARGET="${HOME}/${GO_VERSION}"

mkdir -p "${HOME_BIN}" "${DL_DIR}"

# 1. download if missing
if [ ! -f "${DL_DIR}/${TARBALL}" ]; then
  echo "==> downloading ${URL}"
  curl --fail --silent --show-error --location --output "${DL_DIR}/${TARBALL}" "${URL}"
else
  echo "==> ${DL_DIR}/${TARBALL} already present"
fi

# 2. unpack if missing
if [ ! -x "${TARGET}/bin/go" ]; then
  echo "==> unpacking into ${TARGET}/"
  mkdir -p "${TARGET}"
  tar --strip-components=1 -xzf "${DL_DIR}/${TARBALL}" -C "${TARGET}"
else
  echo "==> ${TARGET}/bin/go already present"
fi

# 3. symlink $HOME/go → $HOME/${GO_VERSION}
if [ -L "${HOME}/go" ] || [ -e "${HOME}/go" ]; then
  current_target="$(readlink "${HOME}/go" || true)"
  if [ "${current_target}" != "${TARGET}" ]; then
    echo "==> retargeting ${HOME}/go → ${TARGET} (was: ${current_target:-<not a symlink>})"
    rm -f "${HOME}/go"
    ln -s "${TARGET}" "${HOME}/go"
  else
    echo "==> ${HOME}/go already points at ${TARGET}"
  fi
else
  echo "==> creating ${HOME}/go → ${TARGET}"
  ln -s "${TARGET}" "${HOME}/go"
fi

# 4. symlink every binary in $HOME/go/bin/ into $HOME/bin/
echo "==> linking $HOME/go/bin/* into ${HOME_BIN}/"
for bin in "${HOME}/go/bin/"*; do
  [ -f "${bin}" ] || continue
  name="$(basename "${bin}")"
  link="${HOME_BIN}/${name}"
  if [ -L "${link}" ] && [ "$(readlink "${link}")" = "${bin}" ]; then
    continue
  fi
  ln -sf "${bin}" "${link}"
  echo "    linked ${link} → ${bin}"
done

echo "==> done. test with:  ~/bin/go version"
