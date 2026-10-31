#!/bin/sh
# remote.network node-agent installer. Downloads the release
# binary for this platform and installs it as ~/.local/bin/remote.
# Releases: https://github.com/dzunic-systems/remote.network/releases
# (REMOTE_RELEASE_BASE overrides, e.g. a pinned .../releases/download/v0.1.0).
set -eu

BASE="${REMOTE_RELEASE_BASE:-https://github.com/dzunic-systems/remote.network/releases/latest/download}"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
case "$OS" in darwin|linux) ;; *) echo "unsupported os: $OS" >&2; exit 1 ;; esac

BIN_DIR="${HOME}/.local/bin"
mkdir -p "$BIN_DIR"
URL="$BASE/remote-$(uname -s | tr '[:upper:]' '[:lower:]')-$ARCH"
# Version-pinned names live in the release; latest/download exposes the
# plain-name aliases.
echo "installing remote ($OS/$ARCH) from $URL"
curl -fsSL "$URL" -o "$BIN_DIR/remote"
chmod +x "$BIN_DIR/remote"
echo "installed: $BIN_DIR/remote"
"$BIN_DIR/remote" --help >/dev/null 2>&1 || echo "note: verify with '$BIN_DIR/remote node status'"
echo "next: remote node start --router wss://<router-host>/node/v1/connect"
