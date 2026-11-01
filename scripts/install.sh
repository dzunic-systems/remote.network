#!/bin/sh
# remote.network node-agent installer for macOS and Linux. Downloads the
# release binary for this platform, checks it against the release's
# checksums.txt and installs it as ~/.local/bin/remote.
#
#   curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.sh | sh
#
# REMOTE_VERSION pins a release (e.g. v0.1.1; default: the latest).
# REMOTE_BIN_DIR installs somewhere else (default: ~/.local/bin).
# Releases: https://github.com/dzunic-systems/remote.network/releases
set -eu

REPO=https://github.com/dzunic-systems/remote.network
if [ -n "${REMOTE_VERSION:-}" ]; then
  BASE="$REPO/releases/download/$REMOTE_VERSION"
else
  BASE="$REPO/releases/latest/download"
fi
BIN_DIR="${REMOTE_BIN_DIR:-$HOME/.local/bin}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in darwin|linux) ;; *) echo "unsupported OS: $OS (Windows: scripts/install.ps1)" >&2; exit 1 ;; esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
# latest/download serves the plain names (remote-<os>-<arch>) next to the
# version-pinned ones; a pinned release has both too.
ASSET="remote-$OS-$ARCH"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "downloading $ASSET from $BASE"
curl -fsSL "$BASE/$ASSET" -o "$TMP/$ASSET"
curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"

# Compared by hand: sha256sum -c is missing on macOS and shasum's list
# format differs between versions.
WANT="$(awk -v a="$ASSET" '$2 == a || $2 == "*" a {print $1}' "$TMP/checksums.txt")"
if command -v sha256sum >/dev/null 2>&1; then
  GOT="$(sha256sum "$TMP/$ASSET" | cut -d' ' -f1)"
else
  GOT="$(shasum -a 256 "$TMP/$ASSET" | cut -d' ' -f1)"
fi
if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
  echo "checksum mismatch for $ASSET: want '$WANT', got '$GOT' — not installed" >&2
  exit 1
fi
echo "checksum OK"

mkdir -p "$BIN_DIR"
install -m 0755 "$TMP/$ASSET" "$BIN_DIR/remote"
# A binary fetched by curl carries no quarantine flag, but one copied in
# from a browser download does; clearing it is harmless either way.
[ "$OS" = darwin ] && xattr -d com.apple.quarantine "$BIN_DIR/remote" 2>/dev/null || true
echo "installed: $BIN_DIR/remote ($("$BIN_DIR/remote" --version 2>/dev/null || echo 'version unknown'))"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo
     echo "note: $BIN_DIR is not on your PATH. Add it, e.g.:"
     echo "  echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.profile   # then open a new shell" ;;
esac

cat <<'NEXT'

next:
  1. sign up at https://spine.remote.network and mint a node token
     (User Settings -> Node Tokens)
  2. remote node bench        # what this machine serves, and how fast
  3. run it as a service (restarts on crash and at boot):
       curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/service.sh | sh
guide: https://github.com/dzunic-systems/remote.network#readme
NEXT
