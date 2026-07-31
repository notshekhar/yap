#!/usr/bin/env bash
# Build the release tarballs.
#
#   scripts/build.sh 0.1.0
#
# One tarball per target, each holding a single static binary, plus a .sha256
# beside it that the installer verifies.
#
# macOS builds need cgo, because the Bluetooth transport is a CoreBluetooth
# binding. Cross-compiling the amd64 slice from an arm64 Mac works because
# Apple's clang targets both — hence the explicit -arch flags. Linux builds have
# no radio (the ble package is a stub there), so they are pure Go and need no
# toolchain at all.

set -euo pipefail

VERSION="${1:-}"
[ -n "$VERSION" ] || { echo "usage: scripts/build.sh <version>" >&2; exit 1; }
VERSION="${VERSION#v}"

cd "$(dirname "$0")/.."
OUT="dist"
rm -rf "$OUT"
mkdir -p "$OUT"

LDFLAGS="-s -w -X main.version=${VERSION}"

sha_of() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else sha256sum "$1" | awk '{print $1}'; fi
}

pack() {
  local target="$1"; shift
  local stage="$OUT/stage/$target"
  mkdir -p "$stage"

  echo "  building $target"
  env "$@" go build -trimpath -ldflags "$LDFLAGS" -o "$stage/yap" ./cmd/yap

  tar -czf "$OUT/yap-${target}.tar.gz" -C "$OUT/stage" "$target"
  sha_of "$OUT/yap-${target}.tar.gz" > "$OUT/yap-${target}.tar.gz.sha256"
}

echo "yap ${VERSION}"

# macOS — the platforms that actually have a radio.
pack darwin-arm64 GOOS=darwin GOARCH=arm64 CGO_ENABLED=1
pack darwin-x64   GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 \
  CGO_CFLAGS="-arch x86_64" CGO_LDFLAGS="-arch x86_64"

# Linux — no Bluetooth yet, but the mesh runs over TCP.
pack linux-x64   GOOS=linux GOARCH=amd64 CGO_ENABLED=0
pack linux-arm64 GOOS=linux GOARCH=arm64 CGO_ENABLED=0

rm -rf "$OUT/stage"
echo
ls -lh "$OUT" | tail -n +2 | awk '{printf "  %-34s %s\n", $9, $5}'
