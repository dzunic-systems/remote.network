# Running a node on Linux

Any 64-bit Linux (amd64 or arm64) runs the agent: it is one static binary
with no dependencies. The service script below needs systemd, which every
mainstream distribution has.

## 1. A backend with a model

The simplest is [Ollama](https://ollama.com/download/linux), which installs
itself as a systemd service:

```sh
curl -fsSL https://ollama.com/install.sh | sh
ollama pull qwen3:8b
```

Pick a model that fits your GPU's VRAM (or system RAM without a GPU):
`qwen3:4b` for 6–8 GB, `qwen3:8b` for 10–12 GB, `qwen3:14b` or
`gemma3:12b` for 16 GB, `qwen3:30b` for 24 GB and up. On NVIDIA, install
the proprietary driver first (`nvidia-smi` must work); the agent reports
the GPU it finds.

**Raise Ollama's context window.** Ollama serves models with a small
window by default, and the network shows consumers the window your node
actually serves. Web search and long chats need room:

```sh
sudo systemctl edit ollama
```

add

```ini
[Service]
Environment="OLLAMA_CONTEXT_LENGTH=16384"
```

and `sudo systemctl restart ollama`. The agent reads the served window
from Ollama and advertises the new one once the model loads again.

For a big GPU box, vLLM or SGLang usually serve faster than Ollama; see
[backends.md](backends.md).

## 2. A node token

Sign up at [spine.remote.network](https://spine.remote.network), open
**User Settings → Node Tokens** and mint one. It starts with `rn_nt_` and
is shown once.

## 3. Install the agent

As the user who will own the node (not root):

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.sh | sh
```

This installs `~/.local/bin/remote` after checking its checksum. If
`~/.local/bin` is not on your PATH, the installer says so and prints the
line to add.

Other options, set before `sh` (they are read by the installer, not by
curl): `REMOTE_VERSION=v0.1.1` pins a release, `REMOTE_BIN_DIR=/usr/local/bin`
installs elsewhere (that one needs `sudo sh`).

Without curl-to-shell: download `remote-linux-amd64` (or `-arm64`) and
`checksums.txt` from the
[releases page](https://github.com/dzunic-systems/remote.network/releases/latest),
then

```sh
grep ' remote-linux-amd64$' checksums.txt | sha256sum -c -
install -m 0755 remote-linux-amd64 ~/.local/bin/remote
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

## 5. Run it as a service (systemd)

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/service.sh | sh
```

Run it as yourself; it asks for sudo to install the unit. It asks for the
token (input hidden), saves it to `~/.remote/node.env` (mode 0600) and
installs `remote-node.service`, a system unit that runs the agent as your
user, starts at boot (after Ollama, when Ollama is a system service) and
restarts it if it exits. Extra flags for `remote node start` go after
`--`:

```sh
curl -fsSL …/service.sh | sh -s -- install -- --model qwen3:8b --max-concurrent 2
```

Re-run it any time to change the flags; it keeps the saved token. On a
machine without a terminal for the prompt, pass the token to `sh`:

```sh
curl -fsSL …/service.sh | REMOTE_NODE_TOKEN=rn_nt_… sh
```

| | |
|---|---|
| Logs | `journalctl -u remote-node -f` |
| State | `systemctl status remote-node` |
| Stop / start | `sudo systemctl stop remote-node` / `sudo systemctl start remote-node` |
| Remove | `curl -fsSL …/service.sh \| sh -s -- uninstall` |

### By hand

What the script writes, if you prefer to manage it yourself (or use a
different init system). The token file, `~/.remote/node.env`
(`chmod 600`):

```sh
NODE_TOKEN=rn_nt_…
ROUTER_ROOT=qnxkjlvbxsshgj6pifmz7pbhx56nv5ar542dmzmp2sxszak7r32a
ROUTER=wss://spine.remote.network/node/v1/connect
```

The unit, `/etc/systemd/system/remote-node.service` (replace `alice` and
the paths with yours):

```ini
[Unit]
Description=remote.network node agent
Wants=network-online.target
After=network-online.target ollama.service

[Service]
User=alice
EnvironmentFile=/home/alice/.remote/node.env
ExecStart=/home/alice/.local/bin/remote node start --router ${ROUTER} --router-root ${ROUTER_ROOT} --node-token ${NODE_TOKEN}
Restart=always
RestartSec=10
# SIGTERM drains in-flight requests (up to 60 s) before the agent exits.
TimeoutStopSec=90

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now remote-node
```

No systemd, or a container? Run the same `remote node start` command under
whatever supervises your processes (OpenRC, runit, supervisord, a
`restart: unless-stopped` container). Keep `~/.remote` on persistent
storage: it holds the node's identity, and a new identity is a new node.

## Update

Re-run the installer, then restart the agent:

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.sh | sh
sudo systemctl restart remote-node
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
