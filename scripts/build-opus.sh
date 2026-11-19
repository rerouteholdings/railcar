#!/usr/bin/env bash
# Builds libopus, the Opus audio codec, as a static library for calls, so
# the railcar binary doesn't need it installed. It downloads the release
# below, checks its SHA-256, and installs it into build/opus.
#
#   scripts/build-opus.sh
#
# env: OPUS_HOST, a configure --host to cross-compile for, such as
#      x86_64-apple-darwin; CC and CFLAGS as configure takes them.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=1.5.2
SHA256=65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1
URL=https://downloads.xiph.org/releases/opus/opus-$VERSION.tar.gz
OUT=$PWD/build/opus
SRC=$PWD/build/opus-src

mkdir -p "$SRC"
TARBALL=$SRC/opus-$VERSION.tar.gz
if [ ! -f "$TARBALL" ]; then
  curl -fsSL -o "$TARBALL.part" "$URL"
  mv "$TARBALL.part" "$TARBALL"
fi
if command -v sha256sum >/dev/null; then
  got=$(sha256sum "$TARBALL" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$TARBALL" | cut -d' ' -f1)
fi
[ "$got" = "$SHA256" ] || { echo "opus-$VERSION.tar.gz has SHA-256 $got, not $SHA256" >&2; rm -f "$TARBALL"; exit 1; }

rm -rf "$SRC/opus-$VERSION" "$OUT"
tar -xzf "$TARBALL" -C "$SRC"
cd "$SRC/opus-$VERSION"
./configure --quiet --disable-shared --enable-static --disable-doc --disable-extra-programs \
  --with-pic --prefix="$OUT" ${OPUS_HOST:+--host=$OPUS_HOST}
make -s -j"$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
make -s install
rm -rf "$OUT/lib/pkgconfig" "$OUT/lib/libopus.la" "$OUT/share"
echo "libopus $VERSION is in build/opus"
