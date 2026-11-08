#!/bin/sh
# Installs the SlicedBread CLI (sb) from the latest GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/melzi-cloud/sliced-bread/main/install.sh | sh
#
# Environment:
#   SB_VERSION       a release tag such as v0.1.0 (default: the latest release)
#   SB_INSTALL_DIR   where to put sb (default: ~/.local/bin)
#
# The binary is checked against the release's SHA256SUMS. That file is also signed with
# the SlicedBread release key; check it yourself with minisign if you want to:
#   minisign -Vm SHA256SUMS -P RWTWEJXhA0VVrEhLJXOwGwqCydkC64znIxzL3NqduaYT7UBbvePx9L0i
set -eu

repo="melzi-cloud/sliced-bread"
dir="${SB_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "install.sh: $*" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported system $(uname -s); sb runs on macOS and Linux" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported CPU $(uname -m)" ;;
esac

if [ -n "${SB_VERSION:-}" ]; then
  base="https://github.com/$repo/releases/download/$SB_VERSION"
else
  base="https://github.com/$repo/releases/latest/download"
fi
asset="sb-$os-$arch"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
echo "downloading $asset from $base"
curl -fsSL -o "$tmp/sb" "$base/$asset" || fail "download failed: $base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || fail "download failed: $base/SHA256SUMS"

want="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")"
[ -n "$want" ] || fail "$asset is not listed in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/sb" | awk '{ print $1 }')"
else
  got="$(shasum -a 256 "$tmp/sb" | awk '{ print $1 }')"
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $asset; not installing"

mkdir -p "$dir"
chmod 755 "$tmp/sb"
mv "$tmp/sb" "$dir/sb"
echo "installed $("$dir/sb" version) to $dir/sb"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: $dir is not on your PATH; add it, for example: export PATH=\"$dir:\$PATH\"" ;;
esac
echo "next: sb init --domain apps.example.com   (see https://github.com/$repo#quickstart)"
