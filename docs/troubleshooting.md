# Troubleshooting

## Where the logs are

| Platform | Service logs |
|---|---|
| macOS | `tail -f ~/.remote/agent.log` |
| Linux | `journalctl -u remote-node -f` |
| Windows | `Get-Content $env:USERPROFILE\.remote\logs\agent.log -Wait -Tail 50 -Encoding UTF8` |

A healthy start logs, in order: `hardware detected`, `backend probed`
(one per backend), `card built` and `bench done` (one per model; the bench
is skipped on later starts thanks to the cache), `serving models=…`, and
`authenticated with router`. After that the agent only logs when something
changes.

`remote node status` prints the node id, the router pin and the last
known state, without contacting anything.

## `no backend could be probed`

The agent found no inference server answering. With default settings it
looks for Ollama on `http://localhost:11434/v1`.

- Is Ollama running? `curl http://localhost:11434/api/tags` should list
  your models. On Windows and macOS start the Ollama app; on Linux
  `sudo systemctl start ollama`.
- Another backend? Check the URL with
  `curl http://localhost:8000/v1/models` (your port), and pass it as
  `--backend-url`, or check `remote backend list`.
- Under a service, the agent starts again by itself (Windows: up to 3
  times, then `Start-ScheduledTask remote-node`).

## `requested model(s) … not served by any backend`

A `--model` you named is not in the backend's list. Use the exact name the
backend uses (`ollama list`; `curl …/v1/models` for others) or pull the
model first.

## `node token rejected`

The router refused `--node-token`: it was mistyped, revoked, or this node
is already claimed by another account. Mint a fresh token under
**User Settings → Node Tokens** and re-run the service script with it
(`REMOTE_NODE_TOKEN=rn_nt_… sh service.sh`; on Windows delete
`%USERPROFILE%\.remote\node-token` and re-run `setup-windows.ps1`).

## `router public key does not match pinned key`

The node pinned a router's key on first contact, and the router it reached
now presents another one. Nodes started with `--router-root` (as the
service scripts do) do not hit this, since they trust the network's root
key rather than one router's. To recover, delete the stale pin and
restart with `--router-root`:

```sh
rm ~/.remote/router_key.pin
```

## The node connects, but my models are missing or look wrong

- **A model is not listed:** embedding models are skipped on backends that
  cannot embed, and a model that fails its probe is skipped; the log says
  which and why. Run once in the foreground with `--reprobe` to probe
  everything again.
- **A small context window:** Ollama's default is small; raise
  `OLLAMA_CONTEXT_LENGTH` (see your platform's guide). For other backends,
  set `--serving-ctx model=tokens`.
- **No tools / vision / thinking badge:** vLLM, llama.cpp and LM Studio
  cannot report these; declare them with `--tool-capable`,
  `--vision-capable`, `--thinking-capable`.

## The session drops every few minutes

The log shows `router session ended` followed by a reconnect, over and
over.

- **A VPN or tunnel on the node.** A full-tunnel VPN has been the cause
  every time we have seen this. Run the node outside the VPN, or exclude
  the agent from it.
- **Sleep.** A machine that sleeps drops its session and rejoins on wake;
  see "Keep the machine available" in your platform's guide.

Occasional reconnects are normal: the agent reconnects with backoff, and
the network routes around a node while it is away.

## Direct path or relay?

The agent reports its NAT type to the router. A cone NAT (most home
routers) gets a direct QUIC path; a symmetric NAT (many mobile and some
corporate networks) stays on the relay, which works the same, with a bit
more latency. Nothing to configure either way; you never need to open a
port.

## Two agents, one identity

Two agents started with the same `~/.remote` take turns kicking each
other off the network. Check for a leftover foreground copy
(`ps aux | grep 'remote node'`, Task Manager on Windows) next to the
service, and stop one of them.

## Still stuck

Open an issue at
[github.com/dzunic-systems/remote.network/issues](https://github.com/dzunic-systems/remote.network/issues)
with your platform, `remote --version`, your backend, and the first 50
lines of the log (it holds no secrets; the token is never logged), or
write to [ping@remote.network](mailto:ping@remote.network).
