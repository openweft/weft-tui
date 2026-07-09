#!/usr/bin/env bash
# install.sh — build, codesign, and install the weft-tui binary.
#
# macOS hangs unsigned Go binaries inside dyld during initial mapping
# (Gatekeeper / library validation). `go build` produces an unsigned
# binary by default, so a `go install` workflow that worked last week
# silently breaks once a fresh build replaces the previously-signed
# copy. This script wraps the three steps that have to happen
# together to ship a working binary :
#
#   1. go build → produces the Mach-O
#   2. codesign -s - → ad-hoc-sign with the system signer (no team ID,
#      no Apple developer cert required — just enough to clear the
#      kernel's "validate every page" check that hangs dyld).
#   3. install -m 0755 → place under ~/.weft/bin (or $PREFIX/bin)
#      so the operator's PATH picks it up.
#
# Override the install location with PREFIX=/usr/local ./install.sh .

set -euo pipefail

PREFIX="${PREFIX:-$HOME/.weft}"
BIN_DIR="${PREFIX}/bin"
GO="${GO:-/usr/local/go/bin/go}"

mkdir -p "${BIN_DIR}"

echo "Building weft-tui ..."
GOWORK=off "${GO}" build -o /tmp/weft-tui .

echo "Codesigning weft-tui ad-hoc ..."
# --force replaces any prior signature ; -s - uses the ad-hoc signer.
# This is enough on macOS 26 to keep dyld from hanging on the binary
# at startup. Skip codesign on non-darwin hosts ; the binary doesn't
# need it on Linux.
if [[ "$(uname -s)" == "Darwin" ]]; then
  codesign -s - --force /tmp/weft-tui
fi

echo "Installing to ${BIN_DIR}/weft-tui ..."
install -m 0755 /tmp/weft-tui "${BIN_DIR}/weft-tui"

echo "Done. sha256:"
shasum -a 256 "${BIN_DIR}/weft-tui" | awk '{print $1}'
