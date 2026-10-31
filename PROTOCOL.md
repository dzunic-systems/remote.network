# Node ↔ router wire protocol (v1)

Single source of truth: `pkg/wire/wire.go`. Envelope:

    {"v":1,"type":"node.heartbeat","node_id":"…","req_id":"…","ts":unix,"data":{…}}

## Control channel (WSS, always)

`GET /node/v1/connect` → handshake: `node.hello` (capability advertisement:
hardware + `models[]` service cards per `wire.ServedModel`) → `node.challenge`
(router signs nonce with the key the node pins — TOFU or `--router-key`;
when the router runs with `-cert`, the challenge also carries the
company-root-signed certificate — see below) →
`node.challenge_response` (node signs) → `node.auth_ok` (heartbeat interval).

Then: `node.heartbeat` every 5s (`in_flight`, `serving_models`; 3 missed →
offline + failover of in-flight). `node.capability_update` re-advertises the
cards without a reconnect (router-side rate limit: 1/minute per session;
earlier updates are dropped with a warning, connection stays up).
`node.draining` (node→router, graceful stop) clears the serving set
immediately. `node.drain` (router→node) is reserved for
router-initiated drain; routers do not send it yet.

## Service cards

Every model a backend probes becomes a card — the static catalog is no
longer a serve gate. Card ids:

- **network tier** — raw ids that map to the embedded catalog keep the
  canonical id (`qwen3:4b` → `qwen/qwen3-4b`); identical models on many
  nodes share the name and the scheduler ranks them.
- **publisher tier** — everything else publishes as `<node_id>/<raw_tag>`
  (the node's own id; the router drops cards namespaced to someone else).

Card facts are node-collected: metadata from the backend where it can speak
(Ollama `/api/show` family/params/quant/ctx, `/api/tags` size), heuristics
off the raw tag where it cannot; `modality` (`chat` | `embedding`) from
backend capabilities with an id-heuristic fallback; prices = operator
`--price-in/out-per-1m` > catalog suggested > the models.yaml `pricing:`
curve.

### Descriptive fields + the model-cards manifest

What no daemon can collect — display name, description, license, lineage,
origin — the operator supplies in a YAML manifest, keyed by raw backend id
or canonical id:

    # ~/.remote/model-cards.yaml (or --model-cards <path>)
    qwen3:4b:
      display_name: "Qwen3 4B (Serbian legal)"
      description: "LoRA fine-tune of qwen3-4b on Serbian legal text"
      base_model: "qwen/qwen3-8b"     # lineage: what this was trained from
      origin: fine-tune               # catalog | fine-tune | self-trained | quantized
      license: apache-2.0
      version: "2026-10-15.1"
      links: ["https://huggingface.co/me/qwen3-4b-sr-legal"]
      languages: ["sr-Latn", "en"]
      weights_sha256: "3a1f…"         # content identity of the served weights

The agent merges it at card-build time (`~/.remote/model-cards.yaml` is
picked up automatically when present; `remote node bench` shows the merged
cards too). The manifest only ever fills the descriptive fields — measured
facts (tps/ttft/ctx/size) and collected metadata (family/quant/params)
stay the agent's. A missing file is normal; a broken file warns and is
ignored (a typo must not brick serving). The managed path stamps
`weights_sha256` itself from the catalog's verified GGUF pin.

Router-side bounds (mechanical, same clamp-never-drop policy):

| field | bound | action |
|---|---|---|
| display_name / license / version | ≤100 chars, trimmed | clamp |
| description | ≤1000 chars | clamp |
| base_model | ≤200 chars, model-id charset | else clear |
| origin | catalog \| fine-tune \| self-trained \| quantized | else clear |
| links | ≤5 http(s) URLs, ≤200 chars each | filter |
| languages | ≤8 tags, ≤16 chars each | filter |
| weights_sha256 | 64 hex (case-normalized) | else clear |

### Pinning one machine (`<node_id>/<raw_tag>` for every card)

A consumer can address a specific node's copy of any served model as
`<node_id>/<raw_tag>` — e.g. `vuqerloo3qzsaqhih4qaun5mic/qwen3:4b`. The
tag is the raw backend id through `wire.RawTag` (last path segment,
`.gguf` stripped, charset-reduced), the same function that names
publisher-tier cards, so node, router, and clients derive one tag from one
raw id. A pinned request schedules only that machine — the key's routing
mode stops mattering — while dispatch, metering, and the ledger stay on
the card's own id. Unknown tag → 404 against that node (never a fallback
to the unpinned model); known-but-offline node → 503 `no_capacity`.

Router-side validation is mechanical only — clamp or drop, never disconnect,
one `card_invalid` node event per offending advertisement:

| bound | value | action |
|---|---|---|
| cards per node | 256 (node self-caps too) | keep first 256 |
| model_id | ≤200 chars, `[A-Za-z0-9._:@/-]`, node-shaped prefix must be own id | drop |
| max_ctx | clamp [512, 2M]; 0 = unknown allowed | clamp |
| prices /1M | clamp [0, 1M] micro-USD | clamp |
| max_concurrent | clamp [1, 64] | clamp |
| modality | chat \| embedding ("" = chat) | else drop |

### Router certificates

A company root keypair — created once on an OFFLINE machine
(`routerd root create`), never stored on a router or the spine — signs each
router's identity key:

    routerd cert issue --root-key root/root.key --router-pub <base32> --expires 8760h
    # → prints the certificate JSON (self-contained: router key, root key,
    #   root id, expiry, serial, root signature)

The router serves it (`routerd -cert cert.json`) inside every
`node.challenge`, and the spine's registry records it (install on a
remote spine: `POST /api/v1/admin/routers/{id}/cert`, validated against
`-root-pub` when the spine sets it). The node side:

- cert present and valid → trust re-anchors at the **company root**: the
  node TOFUs `~/.remote/router_root.pin` from the first cert it accepts
  (or enforces `--router-root <base32>` when the operator pins it
  explicitly). The per-router key pin does not apply — the company can
  rotate router keys without touching nodes.
- cert present but invalid → fatal, no fallback (falling back would mask
  misconfiguration and downgrade).
- no cert → the plain key path byte-for-byte (TOFU or pin of the raw key).

Root-pinning is the operational payoff: every future company router is
trusted with zero node-side action, and router discovery leans
on the same chain (a discovered router is trusted iff its cert chains to
the pinned root).

### Router discovery (`router.directory`)

After `node.auth_ok` the serving router pushes `router.directory`
{routers: [{url, router_id, public_key, cert}]} — every registered router
that published a dial URL (`routerd -public-url`, self-registered at
boot). The node keeps an entry only when its cert chains to the root the
node has pinned; a node with no pinned root keeps nothing, and
uncertified entries are never kept. URL and key alone are data, never
trust.

**Multi-router attach**: the agent runs one session per router
under a fleet supervisor — the `--router` list (repeatable) is the seed
and may be a single URL; certified routers the seeds advertise attach
automatically, deduped by URL and router id, capped at 8 sessions total.
`--router-strict` keeps the world seed-only. Trust never moves: extras
attach only through the pinned root, and an explicit `--router-key` pin
governs the seed alone (a seed's key pin would only mismatch on
siblings). Every session carries its own control channel, punch socket,
and reverse-QUIC dial — NAT mappings are per destination — while the node
identity, serving runtime, and concurrency slots are shared: two routers
can dispatch the same node concurrently, and each sees `busy` (its own
failover trigger) when the slots are full. A router that fails terminally
on a seed kills the agent (the operator must act); a directory-attached
router failing terminally is logged and dropped.

**Fleet changes**: the router registry carries a mutation
counter; the spine re-pushes `router.directory` to every live session
when it changes (watcher, 2s poll) — a new router reaching connected
nodes without a reconnect, a removed one starting the node's removal
clock. A directory-absent extra is detached only after a grace window
(30s): a stale push stays correctable by a newer view, and absence past
the grace means genuinely gone from the registry. Cancelling a session
closes its socket (a quiet router must not hold a detached session open
for a read deadline).

## Inference data plane (over WSS, or one QUIC stream per request)

Same envelopes both paths: `inference.request` (model = card id — canonical
or namespaced; node resolves it via its own cards, enforces per-backend
`max_concurrent`, replies `inference.error{code:"busy"}` = instant failover
trigger) → `inference.chunk` (content/reasoning/finish_reason; reasoning is
billable output) → `inference.done` (node-reported usage for the router's
cross-check — router counts stay authoritative) | `inference.error`. Cancel:
WSS path sends `inference.cancel`; QUIC path resets the stream. Stream EOF
without a terminal event = mid-stream death.

Failover (router-side): pre-first-token failures (busy / dispatch error /
no token within `max(15s, 5×TTFT)`, 60s for unbenched) transparently retry
the next scheduled candidate; post-first-token never retries.

## Embeddings

`inference.embed_request` {model, input[1..16]} → one terminal
`inference.embed_done` {embeddings[][]float32 parallel by index,
input_tokens} | `inference.error`. Both planes carry it (relay and direct
QUIC). Node side: only cards with `modality=embedding` and a backend that
implements the embedder answer (anthropic backends never publish embedding
cards). The batch cap keeps one answer well under the 1 MiB control ceiling.

## Backend registry (node side)

`~/.remote/backends.json` — the dossier provider pattern, plural. One agent
fronts any number of heterogeneous backends
(`remote backend add --id --kind --url [--api-key] [--model …]`,
`list`, `remove`); every probed model of every backend becomes a card under
the agent's one network identity (the provider of record for earnings and
reputation), and each request routes to the card's own backend. Missing
file = one default openai-compatible entry (local Ollama). `--backend-*` flags override the registry for one
run; `--managed` (catalog-pinned GGUF) bypasses it — the pin list is the
catalog's one remaining gate.

## Direct QUIC upgrade

Router is stable+public → observe-and-dial: `punch.begin` {nonce, main/alt
UDP addrs} → node probes BOTH router sockets from ONE UDP socket (0x01-
prefixed JSON beacons; the magic byte keeps quic-go's transport from
classifying them as QUIC) → `punch.observed` (probe sources per socket) →
node classifies NAT (`open`/`cone` = candidate, `symmetric` = relay-only)
→ `punch.report` → router dials QUIC from the MAIN socket (same endpoint
the node punched — port-restricted NATs pass) while the node listens on
its punch socket. QUIC TLS authenticates by identity: self-signed certs
minted from the Ed25519 keys the WSS challenge proved. Heartbeats stay on
WSS forever; a dead QUIC connection falls back to the relay mid-flight.

### Reverse plane (node-dialed, symmetric-NAT escape)

Symmetric NATs are never candidates — the router's dial lands on a mapping
the NAT rebinds per destination. Outbound flows need no traversal: on
every `punch.begin` the node may dial the router's public QUIC endpoint
(the same main addr the punch probes), present its identity certificate,
and serve the same one-stream-per-request protocol. The router maps the
connection to the session by the TLS-presented key and upgrades it — same
`UpgradeQUIC`, same mid-flight relay fallback, ledger path `direct-quic`.
If the punch dial also lands, `UpgradeQUIC`'s swap settles the race; the
loser is closed. `--no-reverse-quic` (or `--no-direct`) keeps a node on
the relay.

## Compatibility

Wire stays v1. Handshake requires exact `v == 1` both directions; after the
handshake, unknown envelope types are logged and skipped by both sides, and
unknown JSON fields inside known envelopes are ignored — additive card
fields and new message types keep old peers alive (an old router answers a
new node's namespaced ids with 404 until upgraded; nothing crashes).
