#!/bin/sh
# Runs the remote.network node agent as a service, so it starts at boot
# (Linux) or login (macOS) and restarts after a crash.
#
#   sh service.sh [install] [-- <extra 'remote node start' flags>]
#   sh service.sh uninstall
#
# Piped straight from GitHub:
#   curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/service.sh | sh
#   curl -fsSL …/service.sh | sh -s -- install -- --model qwen3:8b --max-concurrent 2
#
# Linux: a systemd system unit, remote-node.service, running as you (needs
# sudo to install). macOS: a launchd agent, com.remote-network.node, running
# while you are logged in. Either way the node token lives in
# ~/.remote/node.env (0600), never in the unit or on a command line.
# Re-run it to change flags: an existing token is kept.
#
# Environment: REMOTE_NODE_TOKEN (skip the prompt), REMOTE_ROUTER (default
# below), REMOTE_BIN (default: remote on PATH, else ~/.local/bin/remote).
set -eu

ROUTER="${REMOTE_ROUTER:-wss://spine.remote.network/node/v1/connect}"
# The network's root public key: routers certified by it are trusted, so
# router key rotations need nothing from the node.
ROOT=qnxkjlvbxsshgj6pifmz7pbhx56nv5ar542dmzmp2sxszak7r32a
H="$HOME/.remote"
ENVF="$H/node.env"
UNIT=remote-node
LABEL=com.remote-network.node
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

ACTION=install
if [ $# -gt 0 ] && [ "$1" != "--" ]; then ACTION="$1"; shift; fi
[ $# -gt 0 ] && [ "$1" = "--" ] && shift
FLAGS="$*"

case "$(uname -s)" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) echo "unsupported OS (Windows: scripts/setup-windows.ps1)" >&2; exit 1 ;;
esac
if [ "$(id -u)" = 0 ]; then
  echo "run this as the user who owns the node (not root); it asks for sudo where it needs it" >&2
  exit 1
fi

uninstall() {
  if [ $OS = linux ]; then
    sudo systemctl disable --now $UNIT 2>/dev/null || true
    sudo rm -f /etc/systemd/system/$UNIT.service
    sudo systemctl daemon-reload
  else
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    rm -f "$PLIST"
  fi
  echo "service removed. The node identity and token stay in $H (delete it to forget this node)."
}

if [ "$ACTION" = uninstall ]; then uninstall; exit 0; fi
[ "$ACTION" = install ] || { echo "usage: service.sh [install|uninstall] [-- flags]" >&2; exit 1; }

BIN="${REMOTE_BIN:-$(command -v remote 2>/dev/null || echo "$HOME/.local/bin/remote")}"
[ -x "$BIN" ] || { echo "agent not found at $BIN — run install.sh first (or set REMOTE_BIN)" >&2; exit 1; }
echo "agent:  $BIN ($("$BIN" --version 2>/dev/null || echo 'version unknown'))"
echo "router: $ROUTER"
[ -n "$FLAGS" ] && echo "flags:  $FLAGS"

# The token: from the environment, else the one already saved, else ask.
# The prompt reads the terminal, not stdin, so it works under curl | sh.
TOK="${REMOTE_NODE_TOKEN:-}"
if [ -z "$TOK" ] && [ -f "$ENVF" ]; then
  TOK="$(sed -n 's/^NODE_TOKEN=//p' "$ENVF")"
  [ -n "$TOK" ] && echo "token:  keeping the one in $ENVF"
fi
if [ -z "$TOK" ]; then
  printf 'node token (rn_nt_…, from User Settings -> Node Tokens; input hidden): ' >/dev/tty
  stty -echo </dev/tty
  read -r TOK </dev/tty || true
  stty echo </dev/tty
  echo >/dev/tty
fi
case "$TOK" in rn_nt_*) ;; *) echo "not a node token (they start with rn_nt_)" >&2; exit 1 ;; esac

mkdir -p "$H"
chmod 700 "$H"
( umask 077; printf 'NODE_TOKEN=%s\nROUTER_ROOT=%s\nROUTER=%s\n' "$TOK" "$ROOT" "$ROUTER" > "$ENVF" )
unset TOK

if [ $OS = linux ]; then
  command -v systemctl >/dev/null || { echo "no systemd here — run 'remote node start' under your own supervisor" >&2; exit 1; }
  if [ -d /etc/systemd/system/$UNIT.service.d ]; then
    echo "warning: /etc/systemd/system/$UNIT.service.d exists; a drop-in there can override this unit's ExecStart" >&2
  fi
  # After/Wants on ollama.service order the start when Ollama is a system
  # service here and are ignored when it is not; the agent also re-probes
  # backends that come up later (--retry-backends).
  sudo tee /etc/systemd/system/$UNIT.service >/dev/null <<UNITFILE
[Unit]
Description=remote.network node agent
Wants=network-online.target
After=network-online.target ollama.service

[Service]
User=$(id -un)
EnvironmentFile=$ENVF
ExecStart=$BIN node start --router \${ROUTER} --router-root \${ROUTER_ROOT} --node-token \${NODE_TOKEN} $FLAGS
Restart=always
RestartSec=10
# SIGTERM drains in-flight requests (up to 60 s) before the agent exits.
TimeoutStopSec=90

[Install]
WantedBy=multi-user.target
UNITFILE
  sudo systemctl daemon-reload
  sudo systemctl enable $UNIT >/dev/null 2>&1
  sudo systemctl restart $UNIT
  echo "started $UNIT.service; first start probes and benchmarks every model, which can take a few minutes."
  echo
  echo "logs:    journalctl -u $UNIT -f"
  echo "stop:    sudo systemctl stop $UNIT"
  echo "remove:  sh service.sh uninstall"
else
  # launchd starts the script; the script reads the token file.
  cat > "$H/node-start.sh" <<SH
#!/bin/sh
# Started by launchd ($LABEL); written by service.sh. The token stays in node.env.
. "$ENVF"
exec "$BIN" node start --router "\$ROUTER" --router-root "\$ROUTER_ROOT" --node-token "\$NODE_TOKEN" $FLAGS
SH
  chmod 700 "$H/node-start.sh"
  mkdir -p "$(dirname "$PLIST")"
  cat > "$PLIST" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array><string>/bin/sh</string><string>$H/node-start.sh</string></array>
  <key>KeepAlive</key><true/>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$H/agent.log</string>
  <key>StandardErrorPath</key><string>$H/agent.log</string>
</dict>
</plist>
PL
  plutil -lint "$PLIST" >/dev/null
  # A bootstrap right after a bootout can fail with "Input/output error"
  # while launchd still tears the old job down.
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null && sleep 2 || true
  launchctl bootstrap "gui/$(id -u)" "$PLIST"
  echo "started $LABEL; first start probes and benchmarks every model, which can take a few minutes."
  echo
  echo "logs:    tail -f $H/agent.log"
  echo "stop:    launchctl bootout gui/\$(id -u)/$LABEL"
  echo "remove:  sh service.sh uninstall"
fi
echo "check:   remote node status, and your models in the Catalog at https://spine.remote.network"
