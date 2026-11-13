#!/bin/sh
# Install the gwag binary from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/iodesystems/gwag/main/install.sh | sh
#
# Environment:
#   GWAG_VERSION  release tag to install (default: latest), e.g. v2.12.0
#   PREFIX        install prefix (default: $HOME/.local); binary goes to $PREFIX/bin
set -eu

REPO="iodesystems/gwag"
PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"

err() { echo "install.sh: $*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || err "curl is required"
command -v tar >/dev/null 2>&1 || err "tar is required"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $(uname -s) (releases: linux, darwin)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) err "unsupported architecture: $(uname -m) (releases: amd64, arm64)" ;;
esac

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  err "sha256sum or shasum is required"
fi

tag="${GWAG_VERSION:-}"
if [ -z "$tag" ]; then
  # /releases/latest redirects to /releases/tag/<tag>.
  tag="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")" \
    || err "cannot resolve the latest release"
  tag="${tag##*/}"
fi
case "$tag" in v*) ;; *) tag="v$tag" ;; esac
version="${tag#v}"

archive="gwag_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading $archive ($tag)"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || err "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || err "download failed: $base/checksums.txt"

want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || err "$archive not listed in checksums.txt"
got="$(sha256 "$tmp/$archive")"
[ "$want" = "$got" ] || err "checksum mismatch for $archive (want $want, got $got)"

tar -xzf "$tmp/$archive" -C "$tmp" gwag || err "gwag not found in $archive"
mkdir -p "$BIN_DIR"
# Rename into place: a running gwag keeps its old inode.
cp "$tmp/gwag" "$BIN_DIR/.gwag.new"
chmod 755 "$BIN_DIR/.gwag.new"
mv -f "$BIN_DIR/.gwag.new" "$BIN_DIR/gwag"

echo "Installed gwag $tag to $BIN_DIR/gwag"
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "warning: $BIN_DIR is not on PATH; add it, e.g.  export PATH=\"$BIN_DIR:\$PATH\"" >&2 ;;
esac
