# remote — the remote.network node agent

`remote` turns a computer that already runs a model (Ollama, vLLM,
llama.cpp, or any OpenAI-compatible server) into a node of
[remote.network](https://remote.network): it measures the machine,
advertises the models it serves, connects to the network's routers and
answers the inference requests they send it. Requests run on your
machine's own backend; the agent only carries them.

- **Outbound only.** No port forwarding, no public IP, no VPN. Home NAT
  and CGNAT are fine.
- **Your backend, your models.** The agent serves what you already run,
  and nothing else.
- **One static binary** for macOS, Linux and Windows, on amd64 and arm64.

Status: beta. Earnings are metered per request and shown in your
dashboard; during the beta they are display-only.

## Become a node owner

You need a machine that runs at least one model locally. If it runs
nothing yet, install [Ollama](https://ollama.com) and pull a model that
fits its memory (`ollama pull qwen3:8b` for 16 GB, `qwen3:4b` for 8 GB).

**1. Get a node token.** Sign up at
[spine.remote.network](https://spine.remote.network), open
**User Settings → Node Tokens** and mint a token. It starts with `rn_nt_`
and is shown once. One token can enroll all your machines: every node that
joins with it belongs to your account.

**2. Install the agent.**

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.sh | sh
```

Windows (PowerShell, no admin needed):

```powershell
irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.ps1 | iex
```

Both download the binary from the
[latest release](https://github.com/dzunic-systems/remote.network/releases/latest),
check it against the release's `checksums.txt` and install it into
`~/.local/bin` (`%USERPROFILE%\.local\bin` on Windows).

**3. See what this machine would serve.**

```sh
remote node bench
```

It finds your backend, lists its models and measures time to first token
and tokens per second for each.

**4. Run it as a service**, so it starts at boot or login and restarts
after a crash. The script asks for your token and keeps it in a file only
you can read.

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/service.sh | sh
```

Windows:

```powershell
irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/setup-windows.ps1 | iex
```

The first start probes and benchmarks every model, which can take a few
minutes. Your models then appear in the
[Catalog](https://spine.remote.network) with a live node, and the
earnings of the requests your machine serves show per node under
**User Settings → Usage**.

### Per-platform guides

Each covers requirements, installing a backend, the service in detail
(and how to set it up by hand), keeping the machine awake, updating and
uninstalling:

- [macOS](docs/macos.md) (launchd)
- [Linux](docs/linux.md) (systemd)
- [Windows](docs/windows.md) (scheduled task)

## Backends

With no backend flags the agent serves every model of a local Ollama
(`http://localhost:11434/v1`). Anything else that speaks the OpenAI API
works too: vLLM, llama.cpp's `llama-server`, LM Studio, SGLang, or a
server on another machine on your network.

```sh
remote node start … --model qwen3:8b --model gemma3:12b          # serve only these
remote node start … --backend-url http://localhost:8000/v1        # vLLM instead of Ollama
remote backend add --id vllm --url http://localhost:8000/v1       # several backends at once
```

[docs/backends.md](docs/backends.md) covers each backend, choosing models,
context windows, tool/vision/thinking capabilities, multiple backends and
the zero-setup `--managed` mode.

## Running it by hand

The service scripts run this command; run it in a terminal to try a node
in the foreground (Ctrl-C drains in-flight requests, then exits):

```sh
remote node start \
  --router wss://spine.remote.network/node/v1/connect \
  --router-root qnxkjlvbxsshgj6pifmz7pbhx56nv5ar542dmzmp2sxszak7r32a \
  --node-token rn_nt_…
```

- `--router` is the first router to dial; the agent attaches the network's
  other certified routers by itself.
- `--router-root` is the network's root public key: the agent trusts every
  router certified by it, so router key changes need nothing from you.
- `--node-token` claims the node for your account. A token on the command
  line is visible to other users of the machine in the process list; the
  service scripts read it from a file instead.

`remote node status` prints this node's id, its router pin and the last
known state. `remote node start --help` lists every flag; the ones you are
likely to need are in [docs/backends.md](docs/backends.md#flag-reference).

Run **one** agent per identity: two copies sharing `~/.remote` fight over
the same node. To run two nodes on one machine, give the second one its own
`--home`.

## Problems

[docs/troubleshooting.md](docs/troubleshooting.md) covers the common ones:
no backend found, token refused, router key mismatch, models missing, a
small context window, and how to read the logs.

## Trust

The agent's identity is an Ed25519 key in `~/.remote/node.key`; it signs
the router's challenge on every connect. It trusts routers by key: pin the
network's root key with `--router-root` (recommended), pin one router with
`--router-key`, or with neither it pins the first router it meets.
Requests reach the agent over WSS, upgraded to a direct QUIC connection
when the NAT allows (`--no-direct` keeps the relay). The agent never
accepts inbound connections.

The protocol between nodes and routers is in [PROTOCOL.md](PROTOCOL.md).

## Build from source

```sh
go install remote.network/node/cmd/remote@latest
```

or from a clone:

```sh
make build        # bin/remote
make test
make release      # dist/: every platform, plus checksums.txt
```

Go version: see `go.mod`.

## License

Apache-2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
