#!/usr/bin/env bash
# Builds a Railcar release into dist/release/VERSION/:
#
#   railcar-VERSION-OS-ARCH.tar.gz   the binary, LICENSE, PATENTS.md, README.md, the man page,
#                                    and shell completions, for darwin-arm64, darwin-amd64,
#                                    linux-amd64, and linux-arm64
#   SHA256SUMS                       the tarballs' checksums
#   railcar_*.deb, railcar-*.rpm,    packages for apt, dnf, and pacman, made with nfpm
#   railcar-*.pkg.tar.zst
#
#   scripts/release.sh VERSION       such as 0.1.0 or 0.1.0-rc.1
#
# Railcar links tieroute-mls (Rust) and libopus (C) with cgo, so each system is built
# natively, not cross-compiled from Go alone:
#
# - macOS, on a Mac: arm64 and amd64, with the x86_64-apple-darwin Rust target for amd64
#   (rustup target add x86_64-apple-darwin).
# - Linux, in Docker, through the context in DOCKER_CONTEXT (default colima-reroute-build,
#   started with: colima start --profile reroute-build --arch aarch64 --vm-type vz
#   --vz-rosetta). Each architecture builds in a GO_IMAGE container (default
#   golang:1.27-bookworm) for its platform, which installs Rust with rustup. Without
#   Docker, the Linux tarballs and packages are left out, with a note.
#
# It builds the committed tree (HEAD, or REF), not uncommitted changes, and signs nothing.
# Set ONLY to a space-separated list, such as "darwin-arm64 linux-amd64", to build some.
# env: REF (default HEAD), ONLY, DOCKER_CONTEXT, GO_IMAGE, SOURCE_DATE_EPOCH (default: the
#      last commit's time)
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${1:?usage: scripts/release.sh VERSION}
[[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$ ]] \
  || { echo "$VERSION isn't a version like 0.1.0 or 0.1.0-rc.1" >&2; exit 1; }
NFPM=github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.49.0
REF=${REF:-HEAD}
ONLY=${ONLY:-darwin-arm64 darwin-amd64 linux-amd64 linux-arm64}
DOCKER_CONTEXT=${DOCKER_CONTEXT:-colima-reroute-build}
GO_IMAGE=${GO_IMAGE:-golang:1.27-bookworm}
CARGO=${CARGO:-$(test -x "$HOME/.cargo/bin/cargo" && echo "$HOME/.cargo/bin/cargo" || echo cargo)}
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct "$REF")}
export MACOSX_DEPLOYMENT_TARGET=${MACOSX_DEPLOYMENT_TARGET:-13.0}
[ -z "$(git status --porcelain --untracked-files=no)" ] \
  || echo "note: uncommitted changes aren't in the release; it builds $REF" >&2

OUT=$PWD/dist/release/$VERSION
STAGE=$PWD/dist/release/.stage-$VERSION
rm -rf "$OUT" "$STAGE"
mkdir -p "$OUT" "$STAGE/bin"
trap 'rm -rf "$STAGE"' EXIT
LDFLAGS="-s -w -X main.version=$VERSION"
OPUS_TARBALL=$(ls build/opus-src/opus-*.tar.gz 2>/dev/null | head -1 || true)

# source TARGET: a fresh copy of the tree to build TARGET in, with the libopus
# tarball already there when this checkout has downloaded it.
source_for() {
  local d=$STAGE/src-$1
  mkdir -p "$d"
  git archive "$REF" | tar -x -C "$d"
  if [ -n "$OPUS_TARBALL" ]; then
    mkdir -p "$d/build/opus-src"
    cp "$OPUS_TARBALL" "$d/build/opus-src/"
  fi
  echo "$d"
}

build_darwin() { # arch
  local arch=$1 src rt cc
  [ "$(uname -s)" = Darwin ] || { echo "note: darwin-$arch needs a Mac; left out" >&2; return 0; }
  src=$(source_for "darwin-$arch")
  case $arch in
    arm64) rt=aarch64-apple-darwin; cc="clang -arch arm64" ;;
    amd64) rt=x86_64-apple-darwin; cc="clang -arch x86_64" ;;
  esac
  echo "==> darwin-$arch"
  (cd "$src/mls" && "$CARGO" build --release --locked --target "$rt" --quiet)
  mkdir -p "$src/mls/target/release"
  cp "$src/mls/target/$rt/release/libtieroute_mls.a" "$src/mls/target/release/"
  (cd "$src" && CC="$cc" OPUS_HOST="$rt" scripts/build-opus.sh >"$STAGE/opus-darwin-$arch.log" 2>&1) \
    || { cat "$STAGE/opus-darwin-$arch.log" >&2; exit 1; }
  (cd "$src" && CGO_ENABLED=1 GOOS=darwin GOARCH=$arch CC="$cc" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$STAGE/railcar-darwin-$arch" ./cmd/railcar)
}

build_linux() { # arch
  local arch=$1 src
  src=$(source_for "linux-$arch")
  echo "==> linux-$arch, in $GO_IMAGE on $DOCKER_CONTEXT"
  docker --context "$DOCKER_CONTEXT" run --rm --platform "linux/$arch" \
    -v "$src:/src" -v "$STAGE:/out" -w /src -e LDFLAGS="$LDFLAGS" -e ARCH="$arch" "$GO_IMAGE" bash -ec '
      apt-get update -qq && apt-get install -y -qq curl build-essential >/dev/null
      curl -sSf https://sh.rustup.rs | sh -s -- -y -q --profile minimal >/dev/null
      . "$HOME/.cargo/env"
      (cd mls && cargo build --release --locked --quiet)
      scripts/build-opus.sh >/dev/null 2>/tmp/opus.log || { cat /tmp/opus.log >&2; exit 1; }
      CGO_ENABLED=1 go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" -o /out/railcar-linux-$ARCH ./cmd/railcar'
}

docker_ok() { command -v docker >/dev/null && docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1; }

for t in $ONLY; do
  case $t in
    darwin-*) build_darwin "${t#darwin-}" ;;
    linux-*)
      if docker_ok; then
        build_linux "${t#linux-}"
      else
        echo "note: Docker context $DOCKER_CONTEXT isn't running, so $t is left out. Start it (see the top of this script) and run again with ONLY=$t" >&2
      fi ;;
    *) echo "unknown target $t" >&2; exit 1 ;;
  esac
done

gzip -9n < docs/railcar.1 > "$STAGE/railcar.1.gz"

# Tarballs, for installing by hand.
if tar --version 2>/dev/null | grep -q bsdtar; then
  TAR=(env COPYFILE_DISABLE=1 tar --no-xattrs --no-mac-metadata --uid 0 --gid 0 --uname root --gname root)
else
  TAR=(tar --owner=0 --group=0 --numeric-owner --sort=name --mtime="@$SOURCE_DATE_EPOCH")
fi
built=()
for t in $ONLY; do
  [ -f "$STAGE/railcar-$t" ] || continue
  built+=("$t")
  name=railcar-$VERSION-$t
  d=$STAGE/tar/$name
  mkdir -p "$d/completions"
  install -m 755 "$STAGE/railcar-$t" "$d/railcar"
  install -m 644 LICENSE PATENTS.md README.md docs/railcar.1 "$d/"
  install -m 644 packaging/completions/railcar.bash packaging/completions/_railcar packaging/completions/railcar.fish "$d/completions/"
  find "$d" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} + 2>/dev/null \
    || find "$d" -exec touch -h -t "$(date -u -r "$SOURCE_DATE_EPOCH" +%Y%m%d%H%M.%S)" {} +
  "${TAR[@]}" -czf "$OUT/$name.tar.gz" -C "$STAGE/tar" "$name"
done
[ ${#built[@]} -gt 0 ] || { echo "nothing was built" >&2; exit 1; }
(cd "$OUT" && if command -v sha256sum >/dev/null; then sha256sum railcar-*.tar.gz; else shasum -a 256 railcar-*.tar.gz; fi) > "$OUT/SHA256SUMS"

# Packages for the Linux builds.
linux=()
for t in "${built[@]}"; do [[ $t == linux-* ]] && linux+=("${t#linux-}"); done
if [ ${#linux[@]} -gt 0 ]; then
  echo "==> packages"
  GOBIN=$STAGE/bin go install "$NFPM"
  for arch in "${linux[@]}"; do
    for p in deb rpm archlinux; do
      # pacman's versions can't have a hyphen: 0.1.0-rc.1 is 0.1.0rc1, which sorts before 0.1.0.
      v=$VERSION
      if [ "$p" = archlinux ] && [ "$VERSION" != "${VERSION%%-*}" ]; then pre=${VERSION#*-}; v=${VERSION%%-*}${pre//./}; fi
      ARCH=$arch VERSION=$v STAGE=$STAGE "$STAGE/bin/nfpm" package --config packaging/nfpm.yaml --packager "$p" --target "$OUT/" >/dev/null
    done
  done
fi

ls -1 "$OUT"
echo "Built Railcar $VERSION in $OUT"
