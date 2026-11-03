# Backends: what the agent serves

The agent never runs a model itself (except in `--managed` mode, below). It
forwards requests to an inference server on your machine or your network,
the **backend**, and advertises that server's models to the network. On
startup it probes each model (capabilities, context window), benchmarks
it once (time to first token, tokens per second; cached in
`~/.remote/probe-cache.json`) and publishes a card for it.

## How the agent picks its backends

1. `--managed`: a llama-server the agent downloads and supervises itself.
2. Any of `--backend-url`, `--backend-kind`, `--backend-api-key` given:
   exactly that one backend.
3. Otherwise the registry, `~/.remote/backends.json`, written by
   `remote backend add`.
4. No registry: Ollama on `http://localhost:11434/v1`.

So a machine running Ollama needs no backend configuration at all.

Backend kinds (`--backend-kind`, `remote backend add --kind`):

| Kind | Speaks | Use for |
|---|---|---|
| `openai-compatible` (default) | OpenAI chat completions | Ollama, vLLM, SGLang, llama.cpp, LM Studio, MLX, anything OpenAI-shaped. `ollama`, `vllm` and `lmstudio` are accepted as aliases. |
| `openai` | OpenAI chat completions | The same wire, for servers that follow OpenAI's own API strictly |
| `anthropic` | Anthropic Messages API | Servers that speak the Messages API |

A backend on loopback (`localhost`, `127.0.0.1`) needs no API key. One
elsewhere on your network takes `--backend-api-key` or the environment
variable `REMOTE_BACKEND_API_KEY`.

## Choosing models

Without `--model` the agent serves every model the backend lists, and
re-lists every 10 minutes (`--refresh-models`): a model you pull later is
served without a restart; one you delete disappears from the network.

To serve only some, repeat `--model`:

```sh
remote node start … --model qwen3:8b --model gemma3:12b
```

A model named with `--model` is also loaded into memory at startup and
kept there (`--keep-warm pinned`, the default), so the first request does
not wait for a load. `--keep-warm all` warms every served model, `none`
leaves loading to the backend.

**Model ids.** Models the network knows (its catalog, in
[catalog/models.yaml](../catalog/models.yaml)) are published under their
canonical id, e.g. `qwen3:4b` → `qwen/qwen3-4b`, so a consumer asking for
that model can be routed to any node serving it. Anything else is published
under its own name, pinned to your node.

## Context window

Consumers see the context window your node actually serves, and the
network fits requests (web search results, long chats) into it. The agent
reads it from the backend: Ollama's loaded models, vLLM's `max_model_len`,
llama.cpp's `/props`. For a backend that reports nothing, or to cap it:

```sh
remote node start … --serving-ctx my-model=32768
```

A bigger window is the single most useful thing you can give a small
model. Ollama's default is small; raise it with `OLLAMA_CONTEXT_LENGTH`
(see the [macOS](macos.md), [Linux](linux.md) and [Windows](windows.md)
guides).

## Ollama

The default. Install it from [ollama.com](https://ollama.com), pull a
model, start the agent with no backend flags. The agent reads tool, vision
and thinking support from Ollama itself.

Ollama on another machine in your network (start it there with
`OLLAMA_HOST=0.0.0.0`):

```sh
remote node start … --backend-url http://gpu-box.local:11434/v1
```

## vLLM

```sh
vllm serve Qwen/Qwen3-8B --max-model-len 32768 \
  --enable-auto-tool-choice --tool-call-parser hermes
```

```sh
remote node start … --backend-url http://localhost:8000/v1 \
  --tool-capable Qwen/Qwen3-8B
```

vLLM reports its context window but cannot report whether a model was
started with tool calling, so pass `--tool-capable` for every model you
launched with `--enable-auto-tool-choice` (and `--vision-capable`,
`--thinking-capable` for multimodal and reasoning models). SGLang is set
up the same way, on its own port (default 30000).

## llama.cpp (`llama-server`)

```sh
llama-server -m Qwen3-8B-Q4_K_M.gguf -c 16384 --jinja --port 8081
```

```sh
remote node start … --backend-url http://localhost:8081/v1 \
  --tool-capable Qwen3-8B-Q4_K_M
```

`--jinja` enables the model's chat template and with it tool calling;
pass `--tool-capable` with the model id `llama-server` lists at
`http://localhost:8081/v1/models`. The agent reads the window from
`/props`. (8081, because llama-server's default 8080 is a popular port.)

## LM Studio

In LM Studio, open the Developer tab and start the server (default
port 1234), with the models you want to serve loaded.

```sh
remote node start … --backend-url http://localhost:1234/v1 \
  --serving-ctx qwen3-8b=16384
```

LM Studio does not report the loaded context length over its OpenAI API;
pin it with `--serving-ctx` to the length you loaded the model with.

## MLX (Apple silicon)

```sh
mlx_lm.server --model mlx-community/Qwen3-8B-4bit --port 8082
remote node start … --backend-url http://localhost:8082/v1 \
  --serving-ctx mlx-community/Qwen3-8B-4bit=16384
```

## Several backends at once

One agent can front any number of backends: Ollama for small models, vLLM
for a big one, an older box on the LAN. Register them once:

```sh
remote backend add --id ollama                                # local Ollama (default URL)
remote backend add --id vllm --url http://localhost:8000/v1 --model Qwen/Qwen3-32B
remote backend add --id lan --url http://gpu-box.local:11434/v1
remote backend list
remote backend remove lan
```

and start the agent **without** `--backend-*` flags (they would override
the registry for that run). The registry lives in `~/.remote/backends.json`
(mode 0600, since it can hold API keys). `--model` on `remote backend add`
limits that backend to those models; `--model` on `remote node start`
limits the whole node.

While at least one backend answers, a backend that was down when the
agent started is retried every 2 minutes (`--retry-backends`) and served
once it answers, so you can start and stop backends independently of the
agent. If no backend answers at startup, the agent exits with
`no backend could be probed` and your service manager starts it again.

## Zero-setup: `--managed` (macOS, Linux)

For a machine with nothing installed: the agent downloads llama.cpp's
`llama-server` and a catalog-pinned model file (checked against its
sha256), and runs and supervises it.

```sh
remote node start … --managed --model qwen3:4b
```

It serves exactly one model, and only catalog models with a pinned file
qualify (today: `qwen3:4b`). Files go to `~/.remote`.

## Flag reference

`remote node start --help` prints all of them. The ones node owners use:

| Flag | Default | |
|---|---|---|
| `--router` | localhost (dev) | Always set it. First router to dial: `wss://spine.remote.network/node/v1/connect`. Repeatable. |
| `--router-root` | | The network's root key, `qnxkjlvbxsshgj6pifmz7pbhx56nv5ar542dmzmp2sxszak7r32a`; trusts every router it certified. |
| `--node-token` | | Your `rn_nt_` token: claims the node for your account. |
| `--name` | hostname | Node name shown in your dashboard. |
| `--model` | all | Serve only these models (repeatable). |
| `--max-concurrent` | 1 | Requests served at once. Raise it for backends that batch (vLLM, SGLang, Ollama with `OLLAMA_NUM_PARALLEL`). |
| `--backend-url` / `--backend-kind` / `--backend-api-key` | Ollama | One backend for this run (overrides the registry). |
| `--serving-ctx model=tokens` | from backend | Pin or cap a model's context window. |
| `--tool-capable`, `--vision-capable`, `--thinking-capable` | probed | Declare a capability the backend cannot report. |
| `--keep-warm` | `pinned` | Preload `pinned` (`--model`) models, `all`, or `none`. |
| `--skip-bench` | off | Start serving without benchmarking (cards say unbenched). |
| `--reprobe` | off | Ignore the probe cache: re-probe and re-benchmark every model. |
| `--refresh-models` | 10m | Re-list backends this often (0 = off). |
| `--retry-backends` | 2m | Retry backends that were down at startup (0 = off). |
| `--price-in-per-1m`, `--price-out-per-1m` | catalog | Your price per million tokens, in micro-USD. |
| `--no-direct` | off | Never use the direct QUIC path; always relay. |
| `--home` | `~/.remote` | Agent home: identity, router pin, caches. One per node. |

Problems: [troubleshooting.md](troubleshooting.md).
