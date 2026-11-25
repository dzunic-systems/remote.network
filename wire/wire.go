// Package wire defines the versioned node↔router control-plane envelope.
// It is the single source of truth for both routerd and the node agent;
// Inference dispatch and punch coordination extend this file, never fork it.
package wire

import (
	"encoding/json"
	"errors"
	"time"

	"remote.network/node/identity"
)

// Version is the wire protocol version; a peer receiving a higher version
// must close the connection with a node.error.
const Version = 1

// Envelope types (control plane + inference).
const (
	TypeNodeHello             = "node.hello"
	TypeNodeChallenge         = "node.challenge"
	TypeNodeChallengeResponse = "node.challenge_response"
	TypeNodeAuthOK            = "node.auth_ok"
	TypeNodeAuthFail          = "node.auth_fail"
	TypeNodeHeartbeat         = "node.heartbeat"
	TypeNodeCapabilityUpdate  = "node.capability_update"
	TypeNodeDrain             = "node.drain"
	TypeNodeDraining          = "node.draining"
	TypeNodeReprobe           = "node.reprobe" // router→node: re-characterize models
	TypeNodeError             = "node.error"

	// Router discovery: router→node, pushed after auth_ok.
	TypeRouterDirectory = "router.directory"

	// Inference (over the WSS relay; the same envelopes also ride direct
	// QUIC sessions).
	TypeInferenceRequest = "inference.request" // router→node: generate
	TypeInferenceChunk   = "inference.chunk"   // node→router: one delta
	TypeInferenceDone    = "inference.done"    // node→router: node-reported usage
	TypeInferenceError   = "inference.error"   // node→router: failed (busy, backend error)
	TypeInferenceCancel  = "inference.cancel"  // router→node: consumer went away

	// Embeddings: one round trip, no streaming. A terminal
	// embed_done always answers an embed_request (embed.error is
	// inference.error with the same req_id).
	TypeEmbedRequest = "inference.embed_request" // router→node: embed inputs
	TypeEmbedDone    = "inference.embed_done"    // node→router: vectors + usage

	// Hole punching (all over the WSS control channel — the router is a
	// stable public endpoint, so this is the simple observe-and-dial shape,
	// not a peer-to-peer DCUtR exchange).
	TypePunchBegin    = "punch.begin"    // router→node: punch endpoints + nonce
	TypePunchObserved = "punch.observed" // router→node: what the probes looked like
	TypePunchReport   = "punch.report"   // node→router: NAT class + candidacy

	// Direct client→node path (node v0.1.11+).
	TypeDirectOffer  = "direct.offer"  // router→node: a client is about to dial — open the NAT toward it
	TypeDirectRevoke = "direct.revoke" // router→node: these tickets are dead — refuse and abort
	// On a client-listener stream (client↔node, never the router channel):
	TypeDirectPing    = "direct.ping"    // client→node: reachability check
	TypeDirectPong    = "direct.pong"    // node→client: who answered
	TypeDirectRequest = "direct.request" // client→node: a ticketed inference request
	// Over the node's WSS control channel:
	TypeUsageReceipt    = "usage.receipt"     // node→router: the direct path's signed bill
	TypeUsageReceiptAck = "usage.receipt_ack" // router→node: recorded — the node may delete its copy
)

// DirectPongData answers a direct.ping on a client-listener stream.
type DirectPongData struct {
	NodeID       string `json:"node_id"`
	AgentVersion string `json:"agent_version"`
	Serving      bool   `json:"serving"` // the listener answers direct.request (a later release)
}

// DirectClientALPN is the TLS application protocol of a node's client
// listener — distinct from the router plane's "remote.quic", so neither
// listener can ever be mistaken for the other.
const DirectClientALPN = "remote.client"

// DirectClientInfo is what a node's client listener tells routers in every
// heartbeat: where direct clients can reach it, and through what NAT.
// Absent = the node runs without a client listener (the default).
type DirectClientInfo struct {
	// Endpoints: the router-observed public address first, then the
	// machine's own LAN addresses (for clients on the same network).
	Endpoints []string `json:"endpoints"`
	NAT       string   `json:"nat"`       // open | cone | symmetric | unknown
	MaxConns  int      `json:"max_conns"` // the listener's connection cap
	// Serving says the listener answers direct.request streams; false = it
	// only completes handshakes (reachability checks, node v0.1.11).
	Serving bool `json:"serving,omitempty"`
}

// DirectOfferData asks a node to open its NAT toward a client that is
// about to dial its client listener: the node sends a short burst of UDP
// from the listener socket to each address.
type DirectOfferData struct {
	TicketID    string   `json:"ticket_id,omitempty"` // "" for a reachability check (doctor)
	ClientPub   string   `json:"client_pub"`          // the key the client's TLS certificate will carry
	ClientAddrs []string `json:"client_addrs"`        // its router-observed public address, then LAN addresses
	NotAfter    int64    `json:"not_after"`           // unix seconds; a stale offer is ignored
}

// DirectRevokeData kills tickets on the node: refuse new streams carrying
// them and abort any running one (suspension, key revocation, key limit).
// A push lost to a dead session is bounded by the ticket's lifetime.
type DirectRevokeData struct {
	TicketIDs []string `json:"ticket_ids"`
}

// UsageReceiptAck names the receipts a router recorded — the node may
// delete its queued copies.
type UsageReceiptAck struct {
	TicketID  string `json:"ticket_id"`
	RequestID string `json:"request_id"`
}

// Envelope is the outer wire format for every message in both directions.
type Envelope struct {
	V      int             `json:"v"`
	Type   string          `json:"type"`
	NodeID string          `json:"node_id,omitempty"`
	ReqID  string          `json:"req_id,omitempty"` // inference correlation id
	TS     int64           `json:"ts"`               // unix seconds
	Data   json.RawMessage `json:"data,omitempty"`
}

// New builds an envelope with the current version and clock.
func New(typ, nodeID string, data any) (Envelope, error) {
	return NewReq(typ, nodeID, "", data)
}

// NewReq builds an envelope carrying an inference correlation id.
func NewReq(typ, nodeID, reqID string, data any) (Envelope, error) {
	e := Envelope{V: Version, Type: typ, NodeID: nodeID, ReqID: reqID, TS: time.Now().Unix()}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return e, err
		}
		e.Data = b
	}
	return e, nil
}

// HelloData is the node's first message after the WS upgrade.
type HelloData struct {
	NodeID       string          `json:"node_id"`
	PublicKey    string          `json:"public_key"` // base32, identity.PubString
	Name         string          `json:"name,omitempty"`
	AgentVersion string          `json:"agent_version,omitempty"`
	Capability   json.RawMessage `json:"capability,omitempty"`
	// NodeToken, when set, is an rn_nt_… credential claiming the node for
	// the account that minted it (self-serve ownership; nodes.account_id).
	// Checked only after the challenge proves the identity. Optional —
	// tokenless joins stay valid (unowned, operator-bindable).
	NodeToken string `json:"node_token,omitempty"`
}

// ChallengeData is the router's nonce; RouterSig proves the router holds the
// pinned key (nodes verify against their allowlist / TOFU pin). Cert, when
// present, is the company-root-signed router certificate: nodes
// that pin the root verify the chain and re-anchor trust there — the
// per-router key pin then stops applying, so the company can rotate router
// keys without touching nodes. Absent/nil = pre-phase-B router.
type ChallengeData struct {
	Nonce     string               `json:"nonce"`      // base64, 32 random bytes
	PublicKey string               `json:"public_key"` // router's base32 public key
	RouterID  string               `json:"router_id"`  // identity.ID(router pub)
	RouterSig string               `json:"router_sig"` // Ed25519 signature over the raw nonce
	Cert      *identity.RouterCert `json:"cert,omitempty"`
}

// DirectoryEntry is one discoverable router in a router.directory
// advertisement. A node trusts an entry iff its cert
// chains to the root the node has pinned — URL and key alone are data,
// never trust.
type DirectoryEntry struct {
	URL       string               `json:"url"`       // wss connect URL
	RouterID  string               `json:"router_id"` // identity.ID(public_key)
	PublicKey string               `json:"public_key"`
	Cert      *identity.RouterCert `json:"cert,omitempty"` // absent = uncertified
}

// DirectoryData is the router fleet a node may auto-attach to, pushed by
// the serving router after auth_ok (and refreshed on reconnect). Trust is
// the node's decision, per entry, against its pinned root.
type DirectoryData struct {
	Routers []DirectoryEntry `json:"routers"`
}

// ChallengeResponseData is the node's proof of identity-key possession.
type ChallengeResponseData struct {
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"` // Ed25519 signature over the raw nonce
}

// AuthOKData confirms the session; the node heartbeats at the given interval.
type AuthOKData struct {
	HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
	RouterID           string `json:"router_id"`
}

// AuthFailData explains a rejected handshake.
type AuthFailData struct {
	Reason string `json:"reason"`
	// Code, when set, marks a terminal failure class the agent should not
	// retry (e.g. "node_token" — a refused ownership claim). Empty =
	// transient/generic.
	Code string `json:"code,omitempty"`
}

// HeartbeatData is the node's liveness + load report (5s default).
//
// InFlight counts DISPATCHES the agent holds. Since v0.1.6 it is
// node-wide — every router's traffic to this node — and
// InFlightConn carries this connection's own share, InFlightScope saying
// so. Older agents send no scope: their InFlight counted this connection
// only, which is exactly what InFlightScopeConn names (the counter
// used to live on the per-router client, so a two-spine node reported
// each spine only its own load and no spine saw the others').
//
// Stats, when present, is the sampler's newest machine + backend reading.
// Every field is optional and pointers mean unknown ≠ 0: a
// backend that does not expose running/waiting/KV leaves them out, never
// 0. Routers clamp the object and drop it whole when it breaks bounds —
// the heartbeat itself always counts.
type HeartbeatData struct {
	InFlight      int        `json:"in_flight"`
	InFlightConn  int        `json:"in_flight_conn,omitempty"` // this connection's dispatches (scope node)
	InFlightScope string     `json:"in_flight_scope,omitempty"`
	ServingModels []string   `json:"serving_models,omitempty"`
	Stats         *NodeStats `json:"stats,omitempty"`
	// DirectClient is the client listener's reachability (node v0.1.11+,
	// only when started with --client-direct).
	DirectClient *DirectClientInfo `json:"direct_client,omitempty"`
}

// NodeStats is one 5-second sample of the machine and its backends.
// At is the sample time (RFC3339).
type NodeStats struct {
	At       string         `json:"at"`
	CPU      *CPUStats      `json:"cpu,omitempty"`
	Mem      *MemStats      `json:"mem,omitempty"`
	GPUs     []GPUStats     `json:"gpus,omitempty"`
	Backends []BackendStats `json:"backends,omitempty"`
}

// CPUStats is the machine's CPU: the 1-minute load average, logical
// cores, and — where the platform can measure it — utilization 0..1
// (nil on platforms that cannot, never 0-when-unknown).
type CPUStats struct {
	Load1 float64  `json:"load1"`
	Cores int      `json:"cores"`
	Util  *float64 `json:"util,omitempty"`
}

// MemStats is the machine's memory in MB.
type MemStats struct {
	TotalMB     int64 `json:"total_mb"`
	UsedMB      int64 `json:"used_mb"`
	AvailableMB int64 `json:"available_mb"`
}

// GPUStats is one accelerator's live reading; util and memories are nil
// when the platform cannot measure them (GB10's unified memory reports
// no GPU memory figure — `[N/A]` from nvidia-smi becomes nil).
type GPUStats struct {
	Index      int      `json:"index"`
	Util       *float64 `json:"util,omitempty"` // 0..1
	MemUsedMB  *int64   `json:"mem_used_mb,omitempty"`
	MemTotalMB *int64   `json:"mem_total_mb,omitempty"`
	PowerW     *float64 `json:"power_w,omitempty"`
	TempC      *float64 `json:"temp_c,omitempty"`
}

// BackendStats is one configured backend's live reading. Source says
// where the numbers came from: "metrics" (the backend's own Prometheus
// endpoint), "api" (an admin endpoint, e.g. Ollama /api/ps), or "agent"
// (the agent's own semaphore — held/slots/busy are always known, the
// backend's internal counts are not). Models are the node-side ids this
// backend serves.
type BackendStats struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"` // tensorfold|vllm|sglang|llamacpp|ollama|agent|openai-compatible|…
	Source    string   `json:"source"`
	Models    []string `json:"models"`
	Slots     int      `json:"slots"`                 // the agent's admission slots for this backend
	Held      int      `json:"held"`                  // slots currently held
	Busy1m    int      `json:"busy_1m"`               // ErrBusy refusals in the last minute
	Running   *int     `json:"running,omitempty"`     // the backend's own running count
	Waiting   *int     `json:"waiting,omitempty"`     // queued inside the backend
	KV        *float64 `json:"kv,omitempty"`          // KV-cache use 0..1
	DecodeTPS *float64 `json:"decode_tps,omitempty"`  // token-counter delta
	PrefixHit *float64 `json:"prefix_hit,omitempty"`  // cached/prompt tokens over the interval
	MemUsedMB *int64   `json:"mem_used_mb,omitempty"` // what the backend says it holds
}

// In-flight scope of HeartbeatData.InFlight. A router that sees no scope
// treats the count as connection-scoped (old agent).
const (
	InFlightScopeNode = "node"
	InFlightScopeConn = "conn"
)

// CapabilityUpdateData refreshes hardware/backend info without a reconnect.
type CapabilityUpdateData struct {
	Capability json.RawMessage `json:"capability"`
}

// DrainData tells a node to finish in-flight work and go offline (reserved: routers do not send it yet).
type DrainData struct {
	DeadlineS int `json:"deadline_s"`
}

// ReprobeData asks a node to re-characterize models (router-initiated; the
// agent answers with a fresh node.capability_update). Empty means "re-list
// and claim new models"; All forces re-characterization of everything;
// Models forces specific raw ids. The node rate-limits execution — a
// re-probe re-benches, which is expensive.
type ReprobeData struct {
	All    bool     `json:"all,omitempty"`
	Models []string `json:"models,omitempty"`
}

// ErrorData is the generic error envelope. Inference errors use Code
// "busy" (all concurrency slots taken — instant failover trigger) or
// "backend_error"/"no_handler".
type ErrorData struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// ErrBusy is the sentinel behind ErrorData.Code "busy"; the node's serving
// stack returns it and the ingress maps it onto the wire.
var ErrBusy = errors.New("node at max_concurrent")

// InferenceRequestData is the router→node dispatch. Model is the catalog
// canonical id — the node resolves it to its backend's raw id via its own
// advertisement (it owns the mapping).
type InferenceRequestData struct {
	Model     string             `json:"model"`
	Messages  []InferenceMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens,omitempty"`
	// Tools ride the OpenAI shape (function name/description/parameters);
	// nodes translate for non-OpenAI backends. Empty = plain generation.
	Tools      []ToolSpec `json:"tools,omitempty"`
	ToolChoice string     `json:"tool_choice,omitempty"` // "" | auto | any | none | name:<tool>
	// Reasoning steers thinking models; nil = backend default. Effort is
	// the normalized level (off|low|medium|high), MaxTokens the token
	// budget (Anthropic budget_tokens; best-effort elsewhere).
	Reasoning *ReasoningControl `json:"reasoning,omitempty"`
	// Temperature is the consumer's sampling temperature (0..2); nil =
	// unset, the backend's own default applies; 0 is greedy decoding, as
	// in every OpenAI-compatible API. Backends without a knob ignore it.
	//
	// Until 22 Nov this was a float64 with 0 meaning unset, so a consumer's
	// stated 0 was dropped and the backend sampled at its default (1.0 on
	// the Sparks): dossier's relevance judge, asking for 0, got two replays
	// of the same sections agreeing on 14–62% of their verdicts. A node of
	// before then still reads a 0 as unset; a consumer that must be greedy
	// on every node sends 1e-6, as the canary does.
	Temperature *float64 `json:"temperature,omitempty"`
	// ResponseFormat carries a structured-output constraint: the
	// node's backend enforces it when it can (vLLM guided decoding, Ollama
	// format) and ignores it when it cannot — never a refusal. nil = free
	// generation.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// Priority is the consumer's scheduling hint (e.g. 10 for
	// background work): a vLLM started with --scheduling-policy priority
	// schedules interactive turns ahead of background ones. 0 = unset.
	Priority int `json:"priority,omitempty"`
}

// ResponseFormat is a structured-output constraint on the inference wire.
// Type "json_object" asks for valid JSON; "json_schema" carries the schema
// in JSONSchemaSpec.
type ResponseFormat struct {
	Type       string          `json:"type"` // json_object | json_schema
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

// JSONSchemaSpec is the OpenAI json_schema envelope.
type JSONSchemaSpec struct {
	Name   string          `json:"name,omitempty"`
	Strict bool            `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// ReasoningControl is the normalized thinking-model steering knob.
type ReasoningControl struct {
	Effort    string `json:"effort,omitempty"`     // off | low | medium | high | xhigh
	MaxTokens int    `json:"max_tokens,omitempty"` // budget_tokens on Anthropic
}

// ImagePart is one inline image on a message: raw base64 bytes with a
// media type, or an http(s) URL for backends that fetch.
type ImagePart struct {
	MediaType string `json:"media_type,omitempty"` // image/jpeg | image/png | image/webp | image/gif
	Data      string `json:"data,omitempty"`       // base64, no data: prefix
	URL       string `json:"url,omitempty"`
}

// InferenceMessage is one chat turn on the inference wire. Tool fields
// follow the OpenAI convention: assistant turns may carry ToolCalls (the
// model's invocations), tool results arrive as role "tool" keyed by
// ToolCallID. Images attach alongside the text content (vision models).
type InferenceMessage struct {
	Role       string      `json:"role"`
	Content    string      `json:"content"`
	Images     []ImagePart `json:"images,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

// ToolCall is one tool invocation; Arguments is the arguments object as a
// JSON string (the OpenAI convention the uniform wire keeps).
type ToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolSpec is one consumer-exposed tool; Parameters is the JSON schema.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// InferenceChunkData is one streamed generation event node→router — the
// uniform backend delta, already translated from whatever wire shape the
// backend spoke (reasoning tokens included: they are generated, billable
// output). ToolCalls arrive as complete invocations, emitted by the node
// just before the finish chunk.
type InferenceChunkData struct {
	Content      string     `json:"content,omitempty"`
	Reasoning    string     `json:"reasoning,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
}

// InferenceDoneData is the node's own token accounting, sent once per
// request. The router bills the node's OutputTokens only when the stream it
// relayed could have produced them: no fewer than the chunks that carried
// output, no more than the bytes they spelled (unless the request asked for
// reasoning, which a backend may bill without streaming). Outside those
// bounds it bills its own estimate and records an honesty disagreement. A
// node therefore cannot over-bill past what it delivered, and an honest
// speculative decoder (several tokens a chunk) is billed what it generated.
//
// CachedTokens/ReasoningTokens are detail the backend reported and the
// router stores for the usage log (cache hit rate, thinking share). 0 =
// unreported — a backend that cannot say stays silent; these are display
// aggregates, never billed.
type InferenceDoneData struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Reasoning is set only when the request's thinking control was not met
	// exactly. Older routers ignore it.
	Reasoning *ReasoningOutcome `json:"reasoning,omitempty"`
	// CachedTokens is the prefix-cache hit count among InputTokens (OpenAI
	// prompt_tokens_details.cached_tokens, Anthropic
	// cache_read_input_tokens). ReasoningTokens is the reasoning portion of
	// OutputTokens (OpenAI completion_tokens_details.reasoning_tokens).
	// Both omitempty: old routers ignore them, old nodes send none.
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// Receipt is the node's signed account of this request (node
	// v0.1.11+). Routers verify and record it
	// beside their own count; the direct path will bill by it. Old routers
	// ignore it, old nodes send none.
	Receipt *UsageReceipt `json:"receipt,omitempty"`
}

// ReceiptVersion is the UsageReceipt shape this build signs and verifies.
// A field added to UsageReceipt bumps it: a verifier that does not know
// the new field would re-encode the payload without it and reject the
// signature, so it must refuse the version up front instead.
const ReceiptVersion = 1

// UsageReceipt is a node's signed statement of one served request. The
// node's identity key signs the JSON encoding of every field but Sig
// (clientproto.SignReceipt / VerifyReceipt).
type UsageReceipt struct {
	V               int    `json:"v"`
	TicketID        string `json:"ticket_id,omitempty"` // "" on the router-relayed path (no ticket)
	RequestID       string `json:"request_id"`          // the dispatch's req_id as the node received it
	NodeID          string `json:"node_id"`
	ModelID         string `json:"model_id"` // the model as the node received it
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CachedTokens    int    `json:"cached_tokens,omitempty"`
	ReasoningTokens int    `json:"reasoning_tokens,omitempty"`
	Status          string `json:"status"`   // ok (a failed request sends inference.error and no receipt)
	TTFTms          int    `json:"ttft_ms"`  // node-measured: handler start → first chunk
	TotalMs         int    `json:"total_ms"` // node-measured: handler start → done
	At              int64  `json:"at"`       // unix seconds, node clock
	Sig             string `json:"sig"`      // base64 std Ed25519 signature
}

// ReasoningOutcome says what became of a ReasoningControl the backend could
// not meet: Applied is the nearest level it took, or "default" when the
// backend's own default ran (nothing accepted, or accepted and not obeyed).
type ReasoningOutcome struct {
	Requested string `json:"requested"`
	Applied   string `json:"applied"`
}

// EmbedRequestData asks the node to embed a batch of inputs.
// Input carries 1..16 strings — the batch cap keeps one embed_done well
// under the 1 MiB control-message ceiling even at ~3k dims.
type EmbedRequestData struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// EmbedDoneData answers an embed_request: one vector per input, parallel by
// index. InputTokens is the backend-reported usage (0 = unreported — the
// router then estimates len/4).
type EmbedDoneData struct {
	Embeddings  [][]float32 `json:"embeddings"`
	InputTokens int         `json:"input_tokens"`
}

// PunchBeginData tells the node where to aim probe packets and ties the
// attempt to a nonce (probe payloads carry the same nonce, so the router
// can correlate UDP packets with the authenticated WSS session).
type PunchBeginData struct {
	Nonce      string `json:"nonce"`
	UDPAddr    string `json:"udp_addr"`     // main socket: punch target + QUIC dial source
	AltUDPAddr string `json:"alt_udp_addr"` // second socket, for NAT classification only
}

// PunchObservedData reports the probe sources as the router saw them, one
// per socket. The node classifies its NAT from these (ports of the old
// repo's hole-punching-protocol.md, minus the P2P parts).
type PunchObservedData struct {
	Nonce    string `json:"nonce"`
	MainAddr string `json:"main_addr"`
	AltAddr  string `json:"alt_addr"`
}

// NAT classifications.
const (
	NATOpen      = "open"      // no address translation (same-network / public)
	NATCone      = "cone"      // one mapping for all destinations (any cone flavor)
	NATSymmetric = "symmetric" // per-destination mappings → relay fallback
)

// PunchReportData is the node's verdict: its NAT class and whether the
// router should attempt the direct QUIC dial.
type PunchReportData struct {
	Nonce     string `json:"nonce"`
	NATType   string `json:"nat_type"`
	Candidate bool   `json:"candidate"`
	Detail    string `json:"detail,omitempty"`
}

// ServedModel is one model a node advertises (per-model serving facts).
// The node's serve stack fills it and serializes it into the capability
// blob; the router parses it back out — both sides share this type so the
// JSON contract cannot drift.
type ServedModel struct {
	ModelID       string  `json:"model_id"`
	RawID         string  `json:"raw_id"`
	State         string  `json:"state"` // ready | ready-unbenched | loading | error
	TPS           float64 `json:"tps,omitempty"`
	TTFTms        int     `json:"ttft_ms,omitempty"`
	MaxCtx        int     `json:"max_ctx"`
	MaxConcurrent int     `json:"max_concurrent"`
	PriceInPer1M  int     `json:"price_in_per_1m"`  // micro-USD per 1M tokens; what metering really bills
	PriceOutPer1M int     `json:"price_out_per_1m"` // micro-USD
	// PriceSource says who set the price: "operator" when --price-in or
	// --price-out was given, "default" when the node priced it itself
	// (catalog suggested / curve). Older nodes send nothing; the router
	// then judges by shape (nodelink's network-price intake).
	PriceSource string `json:"price_source,omitempty"`
	BackendKind string `json:"backend_kind"`
	BackendURL  string `json:"backend_url"`

	// Card facts, self-collected by the agent (all optional —
	// omitted on older agents, which the router treats as chat-modality
	// with unknown metadata). Modality "" = chat.
	Modality     string  `json:"modality,omitempty"` // "" | chat | embedding
	Family       string  `json:"family,omitempty"`
	ParamsB      float64 `json:"params_b,omitempty"`
	DefaultQuant string  `json:"default_quant,omitempty"`
	SizeMB       int     `json:"size_mb,omitempty"`
	BackendID    string  `json:"backend_id,omitempty"`
	// Tools is the backend's own function-calling capability (Ollama's
	// /api/show reports it; other backends leave it false = unknown, which
	// the router treats as capable — soft-prefer, never a hard filter).
	Tools bool `json:"tools,omitempty"`

	// Capabilities echoes the backend's own capability claims verbatim
	// (Ollama /api/show: completion, tools, vision, thinking, embedding).
	// Vision/Reasoning are the derived flags the router and UI act on;
	// Capabilities is kept for forward compatibility with future claims.
	Capabilities []string `json:"capabilities,omitempty"`
	Vision       bool     `json:"vision,omitempty"`
	Reasoning    bool     `json:"reasoning,omitempty"`
	// ReasoningEfforts is the effort vocabulary the backend actually
	// accepts — probe-confirmed or --thinking-efforts-pinned, e.g.
	// ["low","medium","xhigh"]. A vLLM qwen3.8 build rejects "high" and
	// "off" outright ("Supported types are xhigh (default), medium, and
	// low"), so consumers need the card's own list, not a guess. Empty =
	// unknown; consumers fall back to the common vocabulary.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	// ServingCtx is the context window the backend is actually serving the
	// model with right now (Ollama /api/ps), as opposed to MaxCtx (the
	// model's training maximum). 0 = not resident / unknown.
	ServingCtx int `json:"serving_ctx,omitempty"`
	// Specialty marks a specialist model that is complete-capable but a
	// poor general chat target ("ocr" today). UIs keep them out of the
	// default chat picker; the scheduler still serves them.
	Specialty string `json:"specialty,omitempty"`

	// Descriptive fields: presentation and
	// provenance metadata that no daemon can collect — the operator
	// supplies them via the node's model-cards manifest (or they stay
	// empty). All optional; older agents omit them and the router's
	// mechanical intake clamps but never editorializes (an unknown origin
	// value is cleared with a card_invalid note, never a dropped card).
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	License     string `json:"license,omitempty"`
	// BaseModel is the lineage: the model this one was trained/derived
	// from ("qwen/qwen3-8b" for a fine-tune). Free of the publisher
	// namespace rule — it names ancestry, not identity.
	BaseModel string `json:"base_model,omitempty"`
	// Origin is how this card's weights relate to the catalog family:
	// catalog | fine-tune | self-trained | quantized. Unknown values are
	// cleared at intake; UIs badge publishers with it.
	Origin  string   `json:"origin,omitempty"`
	Version string   `json:"version,omitempty"`
	Links   []string `json:"links,omitempty"` // https URLs: HF repo, homepage, paper
	// Languages is informational (BCP-47-ish tags, e.g. "en", "sr-Latn").
	Languages []string `json:"languages,omitempty"`
	// WeightsSHA256 is the content identity of the served weights — the
	// honesty anchor for "is this really the model the card claims".
	// Empty or 64 lowercase hex; anything else is cleared at intake.
	WeightsSHA256 string `json:"weights_sha256,omitempty"`
	// WeightsID is the declared source of the weights, one of:
	//   hf:<org>/<name>@<rev40>[+<org>/<name>@<rev40>:<path>]...   (base + overlays)
	//   ollama:sha256:<hex64>                                       (Ollama manifest digest)
	//   gguf:sha256:<hex64>                                         (one GGUF file)
	// Additive on node v0.1.4: old routers ignore it, old nodes send none.
	WeightsID string `json:"weights_id,omitempty"`
	// WeightsFingerprint is sha256 (hex64) over the sorted lines
	// "<path>\t<sha256 hex of that file>\n" of every content file the backend
	// loads. For ollama:/gguf: ids it is the digest itself; empty when the
	// snapshot is not on the node's disk.
	WeightsFingerprint string `json:"weights_fingerprint,omitempty"`
}

// Modality constants for ServedModel.Modality. The empty string means chat
// (older agents predate the field); ModalityOf-style defaults normalize to
// ModalityChat before advertising.
const (
	ModalityChat      = "chat"
	ModalityEmbedding = "embedding"
)

// Advertisement is the typed core of the capability blob: what the machine
// is and which models it serves at what quality/price. Hardware stays raw
// JSON (free-form probe output); Models is the contract.
type Advertisement struct {
	Hardware json.RawMessage `json:"hardware,omitempty"`
	Backend  struct {
		Kind    string `json:"kind"`
		BaseURL string `json:"base_url"`
		Managed bool   `json:"managed"`
	} `json:"backend"`
	Models []ServedModel `json:"models"`
}

// ParseAdvertisement decodes a capability blob; ok is false when the blob is
// not advertisement-shaped (older agents, partial updates).
func ParseAdvertisement(capabilityJSON []byte) (Advertisement, bool) {
	var ad Advertisement
	if err := json.Unmarshal(capabilityJSON, &ad); err != nil || ad.Models == nil {
		return Advertisement{}, false
	}
	return ad, true
}

// Probe is the UDP punch payload: a node's mapping-opener + observation
// beacon, correlated with a WSS-authenticated session by (node_id, nonce).
type Probe struct {
	NodeID string `json:"node_id"`
	Nonce  string `json:"nonce"`
	Seq    int    `json:"seq"`
}

// probeMagic prefixes every probe datagram. The byte matters: quic-go's
// transport classifies datagrams by their first byte, and JSON's leading
// '{' (0x7b) carries the QUIC short-header bit — un-prefixed probes would
// be swallowed as QUIC noise instead of reaching ReadNonQUICPacket.
const probeMagic = 0x01

// EncodeProbe frames a probe for the wire.
func EncodeProbe(p Probe) []byte {
	b, _ := json.Marshal(p)
	return append([]byte{probeMagic}, b...)
}

// DecodeProbe parses a framed probe; ok is false for non-probe datagrams.
func DecodeProbe(b []byte) (Probe, bool) {
	var p Probe
	if len(b) == 0 || b[0] != probeMagic {
		return p, false
	}
	if json.Unmarshal(b[1:], &p) != nil || p.NodeID == "" || p.Nonce == "" {
		return p, false
	}
	return p, true
}
