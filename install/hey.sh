#!/bin/sh
# hey installer for Linux/macOS — https://github.com/kitsyai/hey
#
#   curl -fsSL https://kitsy.ai/hey.sh | sh
#
# Env overrides: HEY_INSTALL_DIR (default /usr/local/bin if writable,
# else ~/.local/bin), HEY_VERSION (default: latest release).
set -eu

REPO="kitsyai/hey"

os=$(uname -s)
case "$os" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *)
    echo "hey installer: unsupported OS '$os' — on Windows use:" >&2
    echo "  irm https://kitsy.ai/hey.ps1 | iex" >&2
    exit 1
    ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "hey installer: unsupported architecture '$arch'" >&2; exit 1 ;;
esac

if [ -n "${HEY_VERSION:-}" ]; then
  ver=${HEY_VERSION#v}
  tag="v$ver"
else
  # Resolve the latest tag via the releases/latest redirect — no API quota,
  # no JSON parsing dependencies.
  tag=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
  tag=${tag##*/}
  ver=${tag#v}
fi
[ -n "$ver" ] && [ "$tag" != "latest" ] || {
  echo "hey installer: could not resolve the latest release" >&2; exit 1;
}

asset="hey_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "hey installer: downloading $asset ($tag)"
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

want=$(grep "  ${asset}\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || { echo "hey installer: no checksum entry for $asset" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
  got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
fi
[ "$got" = "$want" ] || {
  echo "hey installer: checksum mismatch for $asset" >&2
  echo "  want $want" >&2
  echo "  got  $got" >&2
  exit 1
}

tar -xzf "$tmp/$asset" -C "$tmp" hey

dir=${HEY_INSTALL_DIR:-}
if [ -z "$dir" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    dir=/usr/local/bin
  else
    dir="$HOME/.local/bin"
  fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/hey" "$dir/hey"

echo "hey $ver installed to $dir/hey"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: $dir is not on your PATH — add it to your shell profile" ;;
esac
"$dir/hey" version
