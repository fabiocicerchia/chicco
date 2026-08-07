#!/bin/sh
# chicco installer. Detects OS/arch, downloads the matching release archive from
# GitHub, verifies its SHA-256 against the published checksums, and installs the
# binary. Linux and macOS, amd64 and arm64.
#
#   curl -fsSL https://raw.githubusercontent.com/fabiocicerchia/chicco/main/install.sh | sh
#
# Env overrides:
#   CHICCO_VERSION  tag to install (default: latest release)
#   BINDIR          install dir (default: /usr/local/bin, else ~/.local/bin)
set -eu

REPO="fabiocicerchia/chicco"
BIN="chicco"

err() { echo "chicco-install: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux | darwin) ;;
  *) err "unsupported OS '$os' — on Windows download the .zip from https://github.com/$REPO/releases" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) err "unsupported architecture '$arch'" ;;
esac

# Pick a downloader.
if have curl; then dl() { curl -fsSL "$1"; }; dlo() { curl -fsSL -o "$2" "$1"; }
elif have wget; then dl() { wget -qO- "$1"; }; dlo() { wget -qO "$2" "$1"; }
else err "need curl or wget"; fi

version="${CHICCO_VERSION:-}"
if [ -z "$version" ]; then
  version=$(dl "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | head -n1 | cut -d'"' -f4)
  [ -n "$version" ] || err "could not resolve latest version — set CHICCO_VERSION"
fi
num=${version#v}

archive="${BIN}_${num}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "chicco-install: downloading $archive ($version)"
dlo "$base/$archive" "$tmp/$archive" || err "download failed: $base/$archive"
dlo "$base/checksums.txt" "$tmp/checksums.txt" || err "checksums download failed"

# Verify SHA-256.
want=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || err "no checksum for $archive"
if have sha256sum; then got=$(sha256sum "$tmp/$archive" | awk '{print $1}')
elif have shasum; then got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
else err "need sha256sum or shasum to verify the download"; fi
[ "$want" = "$got" ] || err "checksum mismatch — expected $want got $got"

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN" || err "extract failed"

bindir="${BINDIR:-/usr/local/bin}"
if [ ! -w "$bindir" ] && [ -z "${BINDIR:-}" ]; then bindir="$HOME/.local/bin"; fi
mkdir -p "$bindir"
install -m 0755 "$tmp/$BIN" "$bindir/$BIN" 2>/dev/null \
  || { mv "$tmp/$BIN" "$bindir/$BIN" && chmod 0755 "$bindir/$BIN"; } \
  || err "cannot write to $bindir — set BINDIR or re-run with sudo"

echo "chicco-install: installed $bindir/$BIN ($version)"
case ":$PATH:" in *":$bindir:"*) ;; *) echo "chicco-install: add $bindir to your PATH";; esac
