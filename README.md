# remote — the remote.network node agent

`remote` turns a computer that already runs a model (Ollama, vLLM,
llama.cpp, or any OpenAI-compatible server) into a node of
[remote.network](https://remote.network): it measures the machine,
advertises the models it serves, connects to the network's routers and
answers the inference requests they send it. Requests stay on your
machine's own backend; the agent only carries them.

Status: beta.

## Install

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.sh | sh
```

It installs `~/.local/bin/remote` from the
[latest release](https://github.com/dzunic-systems/remote.network/releases/latest).
Windows: download `remote-windows-amd64.exe` from the release and see
`scripts/setup-windows.ps1`, which registers the agent as a scheduled task.

With Go installed:

```sh
go install remote.network/node/cmd/remote@latest
```

## Run a node

With Ollama running locally (the default backend,
`http://localhost:11434/v1`):

```sh
remote node bench                    # what this machine serves, and how fast
remote node start --router wss://spine.remote.network/node/v1/connect
remote node status                   # identity, router pin, last known state
```

Other backends: `--backend-kind openai-compatible --backend-url <url>`,
or register several with `remote backend add` (kept in
`~/.remote/backends.json`). `--managed` downloads and supervises
llama-server with a catalog-pinned model, for machines with nothing
installed yet. `remote node start --help` lists every flag.

Run the agent under a service manager (systemd, launchd, a Windows
scheduled task) so it restarts after a crash, and run **one** agent per
identity: two copies sharing `~/.remote` fight over the same node.

## Trust

The agent's identity is an Ed25519 key in `~/.remote`. It trusts routers
by key: the first router it meets is pinned, or pin explicitly with
`--router-key`, or with `--router-root` to trust every router certified by
the network's root key. Requests reach the agent over WSS, upgraded to a
direct QUIC connection when the NAT allows (`--no-direct` keeps the
relay).

The protocol between nodes and routers is in [PROTOCOL.md](PROTOCOL.md).

## Build

```sh
make build        # bin/remote
make test
make release      # dist/: every platform, plus checksums.txt
```

Go version: see `go.mod`.

## License

Apache-2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
