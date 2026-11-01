# Running a node on macOS

Apple silicon is the best Mac for this: the GPU uses the whole unified
memory, so a 24 GB Mac serves models a 12 GB graphics card cannot. Intel
Macs work too, on the CPU.

## 1. A backend with a model

The simplest is [Ollama](https://ollama.com/download/mac): install the app,
start it once (it then starts at login), and pull a model that fits:

| Unified memory | Try |
|---|---|
| 8 GB | `ollama pull qwen3:4b` |
| 16 GB | `ollama pull qwen3:8b` |
| 24–36 GB | `ollama pull qwen3:14b`, `ollama pull gemma3:27b` |
| 64 GB+ | `ollama pull qwen3:30b` and larger |

**Raise Ollama's context window.** Ollama serves models with a small
window by default, and the network shows consumers the window your node
actually serves. Web search and long chats need room, so give it 16k or
more if memory allows:

```sh
launchctl setenv OLLAMA_CONTEXT_LENGTH 16384
```

then quit Ollama from the menu bar and start it again. (`launchctl setenv`
lasts until reboot; to keep it, set it the same way from a login item, or
run `ollama serve` with the variable under your own launchd agent.)

Using LM Studio, llama.cpp or MLX instead? See [backends.md](backends.md).

## 2. A node token

Sign up at [spine.remote.network](https://spine.remote.network), open
**User Settings → Node Tokens** and mint one. It starts with `rn_nt_` and
is shown once; keep it in your password manager.

## 3. Install the agent

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.sh | sh
```

This installs `~/.local/bin/remote` after checking its checksum. If the
installer says `~/.local/bin` is not on your PATH:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc && exec zsh
```

Pin a version with `REMOTE_VERSION=v0.1.1`, before `sh` (it is read by the
installer, not by curl):

```sh
curl -fsSL …/install.sh | REMOTE_VERSION=v0.1.1 sh
```

Downloaded the binary by hand from the releases page instead? Clear the
browser's quarantine flag, or macOS refuses to run it:

```sh
chmod +x remote-darwin-arm64 && xattr -d com.apple.quarantine remote-darwin-arm64
mv remote-darwin-arm64 ~/.local/bin/remote
```

## 4. Try it

```sh
remote node bench
```

lists the models the agent found and measures each one. Then, to watch a
node join in the foreground:

```sh
remote node start \
  --router wss://spine.remote.network/node/v1/connect \
  --router-root qnxkjlvbxsshgj6pifmz7pbhx56nv5ar542dmzmp2sxszak7r32a \
  --node-token rn_nt_…
```

Look for `authenticated with router` and then `serving models=…`. Ctrl-C
drains in-flight requests and exits.

## 5. Run it as a service (launchd)

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/service.sh | sh
```

It asks for the token (input hidden), saves it to `~/.remote/node.env`
(mode 0600) and installs a launchd agent, `com.remote-network.node`, which
starts at login and restarts the agent if it exits. Extra flags for
`remote node start` go after `--`:

```sh
curl -fsSL …/service.sh | sh -s -- install -- --model qwen3:8b --max-concurrent 2
```

Re-run it any time to change the flags; it keeps the saved token.

| | |
|---|---|
| Logs | `tail -f ~/.remote/agent.log` |
| Stop | `launchctl bootout gui/$(id -u)/com.remote-network.node` |
| Start | `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.remote-network.node.plist` |
| Remove | `curl -fsSL …/service.sh \| sh -s -- uninstall` |

### By hand

What the script writes, if you prefer to manage it yourself. The token
file, `~/.remote/node.env`:

```sh
NODE_TOKEN=rn_nt_…
ROUTER_ROOT=qnxkjlvbxsshgj6pifmz7pbhx56nv5ar542dmzmp2sxszak7r32a
ROUTER=wss://spine.remote.network/node/v1/connect
```

The start script, `~/.remote/node-start.sh` (`chmod 700`):

```sh
#!/bin/sh
. "$HOME/.remote/node.env"
exec "$HOME/.local/bin/remote" node start --router "$ROUTER" --router-root "$ROUTER_ROOT" --node-token "$NODE_TOKEN"
```

The agent, `~/Library/LaunchAgents/com.remote-network.node.plist` (use
your real home path; launchd does not expand `~`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.remote-network.node</string>
  <key>ProgramArguments</key>
  <array><string>/bin/sh</string><string>/Users/you/.remote/node-start.sh</string></array>
  <key>KeepAlive</key><true/>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/Users/you/.remote/agent.log</string>
  <key>StandardErrorPath</key><string>/Users/you/.remote/agent.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.remote-network.node.plist
```

## Keep the Mac available

A launchd agent (like the Ollama app) runs while you are logged in. On a
Mac that serves unattended, such as a Mac mini or Mac Studio in a closet:

- **Log in automatically:** System Settings → Users & Groups → Automatically
  log in as. (Not available with FileVault on; then log in once after
  each reboot.)
- **Don't sleep:** System Settings → Energy (or Battery → Options on a
  laptop) → Prevent automatic sleeping when the display is off. Or
  `sudo pmset -c sleep 0`.
- **Restart after a power cut:** System Settings → Energy → Start up
  automatically after a power failure.

A sleeping node is not lost: the network marks it offline, routes around
it, and it rejoins on wake.

## Update

Re-run the installer, then restart the agent:

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.sh | sh
launchctl kickstart -k gui/$(id -u)/com.remote-network.node
```

The node keeps its identity (`~/.remote/node.key`) across updates.

## Uninstall

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/service.sh | sh -s -- uninstall
rm ~/.local/bin/remote
rm -rf ~/.remote        # forgets this node's identity and token
```

Revoke the token under **User Settings → Node Tokens** if no other machine
uses it. (Revoking stops new machines from joining with it; nodes already
claimed stay yours.)

Problems: [troubleshooting.md](troubleshooting.md).
