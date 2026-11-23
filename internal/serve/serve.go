// Package serve prepares the node's serving stack: probe backends, build a
// service card per model (every probed model is servable; the
// static catalog is not a gate), bench chat cards, and answer the
// router's inference and embedding dispatches.
package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"remote.network/node/catalog"
	"remote.network/node/internal/backend"
	"remote.network/node/internal/bench"
	"remote.network/node/internal/livestats"
	"remote.network/node/internal/managed"
	"remote.network/node/internal/mapping"
	"remote.network/node/internal/weights"
	"remote.network/node/wire"
)

// maxCards self-caps the advertisement at the router's per-node card bound
// (nodelink sanitizeCards), so an honest agent can never trip the hello
// size rejection.
const maxCards = 256

// Options configure Prepare.
type Options struct {
	Home          string
	NodeID        string // publisher namespace for non-catalog cards ("<node_id>/<tag>")
	Backends      []BackendSpec
	Models        []string // requested canonical OR raw ids; empty = all probed
	Managed       bool     // run managed llama-server (single GGUF model)
	SkipBench     bool
	MaxConcurrent int
	PriceInPer1M  int // operator price override (micro-USD/1M): beats catalog + curve
	PriceOutPer1M int

	// CardsManifest is the model-cards manifest path (--model-cards; empty
	// = Home/model-cards.yaml when present). Loaded once in Prepare into
	// Manifest; the merge fills descriptive card fields only (manifest.go).
	CardsManifest string
	Manifest      map[string]CardMeta // preloaded by tests; Prepare loads from the path otherwise

	// Probe-cache controls. A characterized model (metadata + measured
	// tok/s, persisted under Home) is never characterized again — across
	// restarts, reconnects, and periodic re-lists — unless the operator
	// forces it here.
	ForceReprobe bool          // re-probe + re-bench everything, cache ignored
	ProbeModels  []string      // raw ids to (re)probe this run (explicit override)
	RefreshEvery time.Duration // periodic backend re-list; 0 = off (see Runtime.Refresh)

	// AdmitRetryEvery re-probes backends that failed their startup probe
	// this often and admits them once they answer (--retry-backends;
	// 0 = off). Lets the operator start/stop backend services
	// independently of the agent.
	AdmitRetryEvery time.Duration

	// KeepWarm loads the pinned models into backend memory at startup:
	// "pinned" (the --model list / managed model — default), "all", "none".
	KeepWarm string

	// ServingCtxOverrides pins a model's serving context window
	// (--serving-ctx raw=N, repeatable; matches raw OR card id). For
	// backends that report nothing or when the operator knows
	// better than every auto path.
	ServingCtxOverrides map[string]int

	// ToolCapable forces the tools capability on models (--tool-capable
	// raw-or-card-id, repeatable) — for backends that cannot self-report it
	// (vLLM, llama-server) but were launched tool-capable.
	ToolCapable []string
	// VisionCapable / ThinkingCapable: same pin for the other two flags the
	// node cannot detect on such backends (--vision-capable,
	// --thinking-capable).
	VisionCapable   []string
	ThinkingCapable []string
	// EffortsCapable pins a model's accepted reasoning_effort vocabulary
	// outright (--thinking-efforts raw-or-card-id=low,medium,xhigh), where
	// probing cannot run or the operator knows the recipe.
	EffortsCapable map[string]string

	// Weights declares a model's weights identity outright (--weights
	// raw-or-card-id=<org>/<name>@<full-40-hex-commit>
	// [+<org>/<name>@<commit>:<path>]…, repeatable) — the TensorFold path:
	// its /v1/models names no files, so only the operator's word can pin
	// what it loaded. Validated at startup; never guessed here.
	Weights map[string]string
	// HFCache overrides the Hugging Face cache root used for hf: lookups
	// and fingerprints (--hf-cache; default $HF_HUB_CACHE, else
	// $HF_HOME/hub, else ~/.cache/huggingface/hub).
	HFCache string

	Log *slog.Logger

	cache *ProbeCache // wired by Prepare from Home; nil = always probe (tests)
}

// BackendSpec is one inference endpoint to probe and serve (one entry of the
// backend registry; a single --backend-url maps to one spec).
type BackendSpec struct {
	ID     string
	Kind   string
	URL    string
	Key    string
	Models []string // optional per-backend restriction (raw ids)
}

// ServedModel is one service card this node advertises. The shape is the
// shared wire contract (router parses the same JSON out of the capability
// blob).
type ServedModel = wire.ServedModel

// BackendEntry is one live backend with its cards and its own admission
// slots — concurrency is per endpoint, not per node. The busy ring
// counts ErrBusy refusals so the stats object can
// report them per backend: capacity pressure the agent itself saw. It
// lives behind a pointer — entries are copied into rt.Backends.
type BackendEntry struct {
	ID       string
	Kind     string
	URL      string
	Client   backend.Client
	Models   []ServedModel
	restrict []string // per-backend raw-id restriction from the spec
	sem      chan struct{}

	busy *busyRing
}

// busyRing is the 60-second ErrBusy window.
type busyRing struct {
	mu sync.Mutex
	at []time.Time
}

func (r *busyRing) note(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at = append(r.at, now)
	if len(r.at) > 256 { // hard cap; count prunes normally
		r.at = r.at[len(r.at)-256:]
	}
}

// count returns refusals in the last minute, pruning as it goes.
func (r *busyRing) count(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	keep := r.at[:0]
	n := 0
	for _, at := range r.at {
		if now.Sub(at) < time.Minute {
			keep = append(keep, at)
			n++
		}
	}
	r.at = keep
	return n
}

// Runtime is the prepared serving stack. Hold it for the process lifetime.
type Runtime struct {
	Backends []BackendEntry // one per configured backend
	Server   *managed.Server

	// pending: specs that failed their startup probe, re-probed by the
	// admit loop (--retry-backends). Guarded by mu.
	pending []BackendSpec

	// Legacy single-backend view (Backends[0], or the managed server).
	// Kept so `remote node bench` and hand-built test runtimes compile.
	Client      backend.Client
	BackendKind string
	BackendURL  string
	Models      []ServedModel // flattened across backends

	sem chan struct{} // legacy whole-runtime admission (Backends empty)
	log *slog.Logger

	mu    sync.RWMutex // guards Models + Backends[].Models against Refresh
	cache *ProbeCache
	opts  Options
	cat   *catalog.Catalog
	m     *mapping.Mapping

	// Weights-identity state (§2.2–§2.4): the resolved HF cache root and
	// the GGUF digest memo (~/.remote/weights-cache.json). gguf may be nil
	// only in hand-built test runtimes. pendingWeights parks digests that
	// landed while the card views were still being assembled.
	hfCache        string
	gguf           *weights.GGUFHasher
	pendingWeights map[string]ggufWeights

	onChange func() // keep-warm state flips → CLI wires SendCapabilityUpdate
}

// Prepare probes every configured backend and builds a service card per
// model: metadata from the backend where it can speak (Ollama /api/show),
// heuristics off the raw tag where it cannot. Catalog-mapped models keep
// their canonical ids; everything else publishes as "<node_id>/<raw_tag>".
func Prepare(ctx context.Context, opts Options) (*Runtime, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	if opts.Manifest == nil {
		opts.Manifest = loadManifest(manifestPath(opts.Home, opts.CardsManifest), log)
	}

	cat, err := catalog.Default()
	if err != nil {
		return nil, err
	}
	m := mapping.New(cat, nil)

	if opts.Managed {
		if opts.Home != "" {
			opts.cache = LoadProbeCache(filepath.Join(opts.Home, "probe-cache.json"))
		}
		return prepareManaged(ctx, opts, m, log)
	}
	if len(opts.Backends) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	if opts.Home != "" {
		opts.cache = LoadProbeCache(filepath.Join(opts.Home, "probe-cache.json"))
	}

	rt := &Runtime{
		log:     log,
		sem:     make(chan struct{}, max(opts.MaxConcurrent, 1)),
		cache:   opts.cache,
		opts:    opts,
		cat:     cat,
		m:       m,
		hfCache: weights.HFCacheRoot(opts.HFCache),
		gguf:    weights.LoadGGUFHasher(weights.CachePath(opts.Home)),
	}
	var pending []BackendSpec
	for _, spec := range opts.Backends {
		entry, err := rt.prepareBackend(ctx, spec)
		if err != nil {
			rt.Close()
			return nil, err
		}
		if entry == nil {
			pending = append(pending, spec) // probe failed; re-probed by the admit loop
			continue
		}
		rt.Backends = append(rt.Backends, *entry)
	}
	if len(rt.Backends) == 0 {
		rt.Close()
		return nil, fmt.Errorf("no backend could be probed (%d configured)", len(opts.Backends))
	}

	// Flatten, self-cap, dedupe by card id (first backend wins a shared id).
	seen := map[string]bool{}
	for _, e := range rt.Backends {
		for _, card := range e.Models {
			if seen[card.ModelID] {
				log.Warn("duplicate card id across backends; keeping first", "model_id", card.ModelID)
				continue
			}
			seen[card.ModelID] = true
			rt.Models = append(rt.Models, card)
		}
	}
	if len(rt.Models) > maxCards {
		log.Warn("self-cap: advertising only the first 256 cards", "probed", len(rt.Models))
		rt.Models = rt.Models[:maxCards]
		for i := range rt.Backends { // keep entries consistent with the flat list
			rt.Backends[i].Models = filterByIDs(rt.Backends[i].Models, seen)
		}
	}
	// GGUF digests that finished while the card views were being assembled
	// (a small file hashes in milliseconds) land now — see hashGGUFLater.
	rt.flushPendingWeights()
	if len(opts.Models) > 0 && len(rt.Models) == 0 {
		rt.Close()
		return nil, fmt.Errorf("requested model(s) %q not served by any backend", opts.Models)
	}
	opts.cache.Save()

	// Legacy single-backend view: the first entry.
	rt.Client = rt.Backends[0].Client
	rt.BackendKind = rt.Backends[0].Kind
	rt.BackendURL = rt.Backends[0].URL

	if len(rt.Models) == 0 {
		log.Warn("no models to serve — this node will act as a pure router for its backends' LAN")
	}
	if len(pending) > 0 {
		rt.mu.Lock()
		rt.pending = pending
		rt.mu.Unlock()
		if opts.AdmitRetryEvery > 0 {
			log.Warn("backends down at startup — re-probing in the background",
				"backends", specIDs(pending), "every", opts.AdmitRetryEvery)
			go rt.admitLoop(ctx)
		} else {
			log.Info("backends down at startup — no retry (--retry-backends 0)", "backends", specIDs(pending))
		}
	}
	return rt, nil
}

// prepareBackend probes one spec into a BackendEntry; nil, nil means the
// backend could not be probed (config errors still return an error).
func (rt *Runtime) prepareBackend(ctx context.Context, spec BackendSpec) (*BackendEntry, error) {
	opts := rt.opts
	log := rt.log
	kind, err := backend.ParseKind(spec.Kind)
	if err != nil {
		return nil, err
	}
	if kind == "" {
		kind = backend.KindOpenAICompatible
	}
	url := spec.URL
	if url == "" && kind == backend.KindOpenAICompatible {
		url = "http://localhost:11434/v1" // Ollama default
		log.Info("no backend url given; defaulting to Ollama", "backend", spec.ID, "url", url)
	}
	client, err := backend.New(backend.Config{Name: spec.ID, Kind: kind, BaseURL: url, APIKey: spec.Key})
	if err != nil {
		return nil, err // config error: fail loudly
	}

	probed, err := client.ProbeModels(ctx)
	if err != nil {
		log.Warn("backend probe failed — skipping", "backend", spec.ID, "err", err)
		return nil, nil
	}
	log.Info("backend probed", "backend", spec.ID, "models", len(probed))

	_, canEmbed := client.(backend.Embedder)

	entry := &BackendEntry{
		ID:       spec.ID,
		Kind:     string(kind),
		URL:      url,
		Client:   client,
		Models:   []ServedModel{},
		restrict: spec.Models,
		sem:      make(chan struct{}, max(opts.MaxConcurrent, 1)),
		busy:     &busyRing{},
	}
	restrict := setOf(spec.Models)
	global := setOf(opts.Models)
	for _, raw := range probed {
		if len(restrict) > 0 && !restrict[raw] {
			continue
		}
		card := rt.materializeCard(ctx, spec, client, string(kind), raw)
		// The --model filter matches raw OR card id, applied before any
		// benching (never bench what won't be served).
		if len(global) > 0 && !global[card.ModelID] && !global[card.RawID] {
			continue
		}
		if card.Modality == wire.ModalityEmbedding && !canEmbed {
			log.Warn("embedding model skipped: backend cannot embed", "backend", spec.ID, "raw", raw)
			continue
		}
		entry.Models = append(entry.Models, card)
		log.Info("card built",
			"model_id", card.ModelID, "raw", raw, "modality", card.Modality,
			"family", card.Family, "params_b", card.ParamsB, "quant", card.DefaultQuant,
			"ctx", card.MaxCtx, "size_mb", card.SizeMB, "state", card.State,
			"price_in", card.PriceInPer1M, "price_out", card.PriceOutPer1M,
			"catalog_mapped", !isNamespaced(opts.NodeID, card.ModelID),
		)
	}
	if len(global) > 0 && len(entry.Models) == 0 {
		log.Warn("backend serves none of the requested models", "backend", spec.ID)
	}
	if len(entry.Models) > 0 {
		opts.cache.Save()
	}
	return entry, nil
}

// materializeCard characterizes one backend model into a service card —
// the single path used by startup, refresh, and forced re-probes. A cache
// hit (characterized before, not forced, not explicitly requested) reuses
// the stored facts verbatim: no ProbeMeta call, no bench. A miss probes,
// benches (unless skipped), and lands in the cache.
func (rt *Runtime) materializeCard(ctx context.Context, spec BackendSpec,
	client backend.Client, kind, raw string) ServedModel {
	opts := rt.opts
	log := rt.log
	key := spec.ID + "|" + raw
	forced := false
	for _, want := range opts.ProbeModels {
		if want == raw {
			forced = true
			break
		}
	}
	if cached, hit := rt.cache.Get(key); hit && !opts.ForceReprobe && !forced {
		meta := backend.Meta{
			Family: cached.Family, Quant: cached.Quant, ParamsB: cached.ParamsB,
			Ctx: cached.Ctx, SizeBytes: int64(cached.SizeMB) << 20,
			Capabilities: cached.Capabilities, Efforts: cached.Efforts,
		}
		// What the listing states is re-read on every hit: it was fetched
		// this start anyway, and an entry written by an older agent may
		// predate the fields (v0.1.3 → v0.1.4 lost the Sparks' thinking).
		if lf, ok := client.(backend.ListingFacter); ok {
			if thinking, efforts, ok := lf.ListingFacts(raw); ok {
				if thinking && !slices.Contains(meta.Capabilities, "thinking") {
					meta.Capabilities = append(slices.Clone(meta.Capabilities), "thinking")
				}
				if len(efforts) > 0 {
					meta.Efforts = efforts
				}
				if !slices.Equal(meta.Capabilities, cached.Capabilities) || !slices.Equal(meta.Efforts, cached.Efforts) {
					cached.Capabilities, cached.Efforts = meta.Capabilities, meta.Efforts
					rt.cache.Put(key, cached)
				}
			}
		}
		card := buildCard(opts, rt.cat, rt.m, spec, raw, meta, cached.Modality, kind)
		card.State = cached.State
		card.TPS = cached.TPS
		card.TTFTms = cached.TTFTms
		if card.WeightsID != "" {
			// An operator declaration (applied in buildCard) wins over the
			// cached facts; its fingerprint is re-walked, cheap.
			card.WeightsFingerprint = rt.hfFingerprint(&card)
		} else {
			// Never replay a cached identity: the cache key is backend+raw
			// tag, which survives `ollama pull` of new weights under the
			// same tag. Every provenance tier is cheap to re-ask (one
			// /api/tags, the listing already fetched, the GGUF memo keyed
			// by path+size+mtime), so the card states what is on disk now.
			rt.applyProvenanceWeights(ctx, client, &card, key)
		}
		rt.applyServingCtx(&card, client, raw)
		rt.applyDetectedCapabilities(ctx, &card, client)
		log.Info("card cached — skipping probe/bench", "backend", spec.ID, "raw", raw,
			"state", card.State, "probed_at", cached.ProbedAt)
		return card
	}

	meta := backend.HeuristicMeta(raw)
	if prober, ok := client.(backend.Prober); ok {
		if pm, err := prober.ProbeMeta(ctx, raw); err == nil {
			meta = pm
		}
	}
	modality := backend.ModalityOf(raw, meta)
	card := buildCard(opts, rt.cat, rt.m, spec, raw, meta, modality, kind)

	if modality == wire.ModalityChat && !opts.SkipBench {
		log.Info("benchmarking", "model", card.ModelID, "raw", raw)
		bctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		res, err := bench.Run(bctx, client, raw)
		cancel()
		if err != nil {
			log.Warn("bench failed — advertising unbenched", "model", card.ModelID, "err", err)
		} else {
			card.State = "ready"
			card.TPS = res.TokPerSec
			card.TTFTms = res.TTFTp50ms
			log.Info("bench done", "model", card.ModelID, "ttft_ms", res.TTFTp50ms,
				"tok_s", fmt.Sprintf("%.1f", res.TokPerSec))
		}
	} else if modality == wire.ModalityEmbedding {
		card.State = "ready" // no chat bench for embedders
	}
	rt.applyServingCtx(&card, client, raw)

	// Weights identity (§2.2): the operator declaration wins; otherwise
	// what the backend can prove about its own files, precedence 2–4. The
	// fingerprint walk is cheap and runs on every (re)probe, no cache;
	// GGUF hashing never blocks (§2.4).
	if card.WeightsID != "" {
		card.WeightsFingerprint = rt.hfFingerprint(&card)
	} else {
		rt.applyProvenanceWeights(ctx, client, &card, key)
	}

	if rt.cache != nil {
		cached := CachedModel{
			RawID: raw, Modality: modality, State: card.State,
			Family: meta.Family, Quant: meta.Quant, ParamsB: meta.ParamsB,
			Ctx: meta.Ctx, SizeMB: int(meta.SizeBytes >> 20),
			Capabilities: meta.Capabilities, Efforts: meta.Efforts,
			WeightsID: card.WeightsID, WeightsFingerprint: card.WeightsFingerprint,
			TPS: card.TPS, TTFTms: card.TTFTms, ProbedAt: nowStamp(),
		}
		if card.State == "ready" && card.TPS > 0 {
			cached.BenchedAt = nowStamp()
		}
		rt.cache.Put(key, cached)
	}
	rt.applyDetectedCapabilities(ctx, &card, client)
	return card
}

// hfFingerprint computes the §2.3 fingerprint for a card that carries an
// hf: WeightsID. Empty when the snapshot is not on this machine (the
// expected containerized-backend case) or when a declared overlay file is
// missing/unreadable — warned once per card, with the path.
func (rt *Runtime) hfFingerprint(card *ServedModel) string {
	return weights.Fingerprint(rt.hfCache, card.WeightsID, func(path, reason string) {
		rt.log.Warn("weights fingerprint incomplete — publishing the id without one",
			"model", card.ModelID, "path", path, "reason", reason)
	})
}

// applyProvenanceWeights fills a card's weights identity from what the
// backend itself can say about its files, §2.2 precedence 2–4: the Ollama
// manifest digest, the vLLM root path, the llama.cpp /props model_path.
// A GGUF file that is readable but not yet hashed publishes without an id
// and gets one via a capability update once the background hash lands
// (§2.4) — startup and bench never wait on tens of GB.
func (rt *Runtime) applyProvenanceWeights(ctx context.Context, client backend.Client, card *ServedModel, cacheKey string) {
	wp, ok := client.(backend.WeightsProvenance)
	if !ok {
		return
	}
	digest, root, path := wp.WeightsFacts(ctx, card.RawID)

	// 2. Ollama manifest digest: identity and fingerprint in one.
	if id, fp, ok := weights.Ollama(digest); ok {
		card.WeightsID, card.WeightsFingerprint = id, fp
		return
	}
	// 3. A listed root path naming a HF-cache snapshot — any prefix (the
	// container's /root/.cache/... counts): the rev comes off the path, so
	// it is exact. A bare org/name resolves through the local refs/main,
	// and a local dir resolves to nothing at all. Never a guessed rev.
	if root != "" {
		if id := weights.HFIDFromPath(root); id != "" {
			card.WeightsID = id
			card.WeightsFingerprint = rt.hfFingerprint(card)
			return
		}
		if id := weights.HFIDFromRepo(rt.hfCache, root); id != "" {
			card.WeightsID = id
			card.WeightsFingerprint = rt.hfFingerprint(card)
			return
		}
	}
	// 4. llama.cpp's loaded GGUF. A snapshot path folds back into tier 3;
	// otherwise the file hashes (memoized; off the startup path).
	if path != "" {
		if id := weights.HFIDFromPath(path); id != "" {
			card.WeightsID = id
			card.WeightsFingerprint = rt.hfFingerprint(card)
			return
		}
		if hex, ok := rt.gguf.Cached(path); ok {
			card.WeightsID, card.WeightsFingerprint = "gguf:sha256:"+hex, hex
			return
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
			rt.hashGGUFLater(card.ModelID, path, cacheKey)
		}
	}
}

// hashGGUFLater digests a GGUF in the background, then moves the id onto
// the live card (both views) and the probe cache, and fires the change
// hook the CLI wires to SendCapabilityUpdate. A digest that lands while
// the card views are still being built (Prepare assembly, a concurrent
// Refresh) is parked in pendingWeights and applied when the cards exist
// — never dropped. Lost races beyond that (a refresh dropped the card)
// just log; the next probe answers from the digest memo instantly.
func (rt *Runtime) hashGGUFLater(modelID, file, cacheKey string) {
	rt.log.Info("hashing gguf weights in the background — the card publishes without a weights id until it lands",
		"model", modelID, "file", file)
	go func() {
		hex, err := rt.gguf.HashFile(file)
		if err != nil {
			rt.log.Warn("gguf hash failed — the card stays without a weights id",
				"model", modelID, "file", file, "err", err)
			return
		}
		id, fp := "gguf:sha256:"+hex, hex
		if rt.setWeights(modelID, id, fp) {
			rt.changed() // → SendCapabilityUpdate: the router sees the id land
		} else {
			// The card views did not exist yet (a tiny file hashing while
			// Prepare was still assembling) — park it for the flush.
			rt.mu.Lock()
			if rt.pendingWeights == nil {
				rt.pendingWeights = map[string]ggufWeights{}
			}
			rt.pendingWeights[modelID] = ggufWeights{id: id, fp: fp}
			rt.mu.Unlock()
		}
		if rt.cache != nil {
			if c, ok := rt.cache.Get(cacheKey); ok {
				c.WeightsID, c.WeightsFingerprint = id, fp
				rt.cache.Put(cacheKey, c)
			}
		}
		rt.log.Info("gguf weights hashed — capability update carries the id",
			"model", modelID, "sha256", hex)
	}()
}

// ggufWeights is one background digest waiting for its card to exist.
type ggufWeights struct{ id, fp string }

// flushPendingWeights applies parked digests and clears them; reports
// whether any landed so the caller counts it as a card change. Must
// never run under rt.mu — it fires changed(), which reads the cards.
func (rt *Runtime) flushPendingWeights() bool {
	rt.mu.Lock()
	applied := rt.flushPendingWeightsLocked()
	rt.mu.Unlock()
	if applied {
		rt.changed()
	}
	return applied
}

func (rt *Runtime) flushPendingWeightsLocked() bool {
	applied := false
	for modelID, u := range rt.pendingWeights {
		if rt.setWeightsLocked(modelID, u.id, u.fp) {
			applied = true
		}
		delete(rt.pendingWeights, modelID)
	}
	return applied
}

// setWeights moves a weights identity onto every view of one card, under
// the same lock Refresh uses. Returns whether anything changed.
func (rt *Runtime) setWeights(modelID, id, fp string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.setWeightsLocked(modelID, id, fp)
}

func (rt *Runtime) setWeightsLocked(modelID, id, fp string) bool {
	changed := false
	for i := range rt.Models {
		if rt.Models[i].ModelID == modelID &&
			(rt.Models[i].WeightsID != id || rt.Models[i].WeightsFingerprint != fp) {
			rt.Models[i].WeightsID, rt.Models[i].WeightsFingerprint = id, fp
			changed = true
		}
	}
	for bi := range rt.Backends {
		for i := range rt.Backends[bi].Models {
			if rt.Backends[bi].Models[i].ModelID == modelID &&
				(rt.Backends[bi].Models[i].WeightsID != id || rt.Backends[bi].Models[i].WeightsFingerprint != fp) {
				rt.Backends[bi].Models[i].WeightsID, rt.Backends[bi].Models[i].WeightsFingerprint = id, fp
				changed = true
			}
		}
	}
	return changed
}

// applyDetectedCapabilities folds probe-confirmed capability facts into a
// card (caps.go's CapabilityProber): a vLLM launched with a tool parser or
// a vision model ends up tools/vision true without an operator pin, and
// cached cards get the same treatment on every start — no --reprobe.
// Detection only ever sets true (a 200 is evidence; a 4xx is left to the
// card default), so it composes with, and never overrides, the operator's
// --tool-capable/--vision-capable pins. Chat cards only — embedders would
// just 400 both probes.
func (rt *Runtime) applyDetectedCapabilities(ctx context.Context, card *ServedModel, client backend.Client) {
	if card.Modality != wire.ModalityChat {
		return
	}
	prober, ok := client.(backend.CapabilityProber)
	if !ok {
		return
	}
	d := prober.DetectCapabilities(ctx, card.RawID)
	set := func(name string, target *bool) {
		if !*target {
			*target = true
			for _, c := range card.Capabilities {
				if c == name {
					return
				}
			}
			card.Capabilities = append(card.Capabilities, name)
		}
	}
	if d.Tools {
		set("tools", &card.Tools)
		rt.log.Info("capability detected by probe", "backend", card.BackendID, "raw", card.RawID, "cap", "tools")
	}
	if d.Vision {
		set("vision", &card.Vision)
		rt.log.Info("capability detected by probe", "backend", card.BackendID, "raw", card.RawID, "cap", "vision")
	}
	if d.Thinking {
		set("thinking", &card.Reasoning)
		rt.log.Info("capability detected by probe", "backend", card.BackendID, "raw", card.RawID, "cap", "thinking")
	}
	// The effort vocabulary rides the thinking detection (only thinking
	// models were probed for it). Detection only ever ADDS facts: a card
	// already carrying a pinned vocabulary keeps it.
	if len(d.Efforts) > 0 && len(card.ReasoningEfforts) == 0 {
		card.ReasoningEfforts = d.Efforts
		rt.log.Info("reasoning efforts detected by probe", "backend", card.BackendID, "raw", card.RawID, "efforts", strings.Join(d.Efforts, ","))
	}
}

// ForceReprobe is the router-initiated re-probe (node.reprobe): re-list
// plus, when all, re-characterize everything; models narrows the force to
// specific raw ids. Cache discipline is the same as the operator flags —
// forced models re-probe, the rest ride the cache. Returns whether the
// serving set changed (caller re-advertises).
func (rt *Runtime) ForceReprobe(ctx context.Context, all bool, models []string) bool {
	oldForce, oldList := rt.opts.ForceReprobe, rt.opts.ProbeModels
	rt.opts.ForceReprobe, rt.opts.ProbeModels = all, models
	defer func() { rt.opts.ForceReprobe, rt.opts.ProbeModels = oldForce, oldList }()
	return rt.Refresh(ctx)
}

// Refresh re-lists every backend and claims only what changed: models new
// to this node are characterized (cache permitting) and added; models no
// longer listed are dropped. Known models are never re-probed — cache and
// current cards are the facts. A backend whose listing fails keeps its
// current set (a flaky endpoint must not zero the node). Returns whether
// the serving set changed — the caller re-advertises via
// Client.SendCapabilityUpdate, whose next hello covers reconnects.
func (rt *Runtime) Refresh(ctx context.Context) bool {
	if len(rt.Backends) == 0 {
		return false
	}
	rt.mu.Lock()
	changed := rt.refreshLocked(ctx)
	rt.mu.Unlock()
	// GGUF digests that finished while the write lock was held (a small
	// file hashing mid-refresh) land now, outside the lock — changed()
	// reads the cards. They are a card change worth re-advertising even
	// when the re-list saw none.
	return rt.flushPendingWeights() || changed
}

// refreshLocked is Refresh's body; rt.mu is held for writing.
func (rt *Runtime) refreshLocked(ctx context.Context) bool {
	changed := false
	for bi := range rt.Backends {
		e := &rt.Backends[bi]
		probed, err := e.Client.ProbeModels(ctx)
		if err != nil {
			rt.log.Warn("refresh probe failed — keeping current set", "backend", e.ID, "err", err)
			continue
		}
		live := setOf(probed)
		restrict := setOf(e.restrict)
		var kept []ServedModel
		for i := range e.Models {
			if live[e.Models[i].RawID] {
				if rt.applyServingCtx(&e.Models[i], e.Client, e.Models[i].RawID) {
					changed = true // the backend was relaunched with a different window
				}
				kept = append(kept, e.Models[i])
			} else {
				changed = true
				rt.log.Info("refresh: model gone from backend — dropping", "backend", e.ID, "raw", e.Models[i].RawID)
			}
		}
		e.Models = kept
		for _, raw := range probed {
			if len(restrict) > 0 && !restrict[raw] {
				continue
			}
			known := false
			for _, m := range e.Models {
				if m.RawID == raw {
					known = true
					break
				}
			}
			if known {
				continue
			}
			card := rt.materializeCard(ctx, BackendSpec{ID: e.ID, Kind: e.Kind, URL: e.URL, Models: e.restrict},
				e.Client, e.Kind, raw)
			e.Models = append(e.Models, card)
			changed = true
			rt.log.Info("refresh: new model claimed", "backend", e.ID, "raw", raw, "model_id", card.ModelID)
		}
	}
	if !changed {
		return false
	}
	rt.rebuildFlatLocked()
	if rt.cache != nil {
		rt.cache.Save()
	}
	return true
}

// rebuildFlatLocked re-derives the flat card view from the per-backend
// view: the same dedupe (first backend wins a shared id) and self-cap
// rules as Prepare. Callers hold mu for writing.
func (rt *Runtime) rebuildFlatLocked() {
	seen := map[string]bool{}
	rt.Models = rt.Models[:0]
	for _, e := range rt.Backends {
		for _, card := range e.Models {
			if seen[card.ModelID] {
				continue
			}
			seen[card.ModelID] = true
			rt.Models = append(rt.Models, card)
		}
	}
	if len(rt.Models) > maxCards {
		rt.Models = rt.Models[:maxCards]
		for bi := range rt.Backends {
			rt.Backends[bi].Models = filterByIDs(rt.Backends[bi].Models, seen)
		}
	}
}

// buildCard assembles one service card: identity (canonical vs namespaced),
// metadata (measured > catalog > heuristic), price (operator > catalog
// suggested > default curve), and the wire's backend fields.
func buildCard(opts Options, cat *catalog.Catalog, m *mapping.Mapping, spec BackendSpec,
	raw string, meta backend.Meta, modality, backendKind string) ServedModel {
	mod, mapped := m.Resolve(raw)

	card := ServedModel{
		RawID:         raw,
		State:         "ready-unbenched",
		MaxConcurrent: max(opts.MaxConcurrent, 1),
		PriceInPer1M:  opts.PriceInPer1M,
		PriceOutPer1M: opts.PriceOutPer1M,
		BackendKind:   backendKind,
		BackendURL:    "local",
		BackendID:     spec.ID,
		Modality:      modality,
		Family:        meta.Family,
		ParamsB:       meta.ParamsB,
		DefaultQuant:  meta.Quant,
		SizeMB:        int(meta.SizeBytes / (1 << 20)),
	}
	if len(meta.Capabilities) > 0 {
		card.Capabilities = make([]string, 0, len(meta.Capabilities))
		for _, cap := range meta.Capabilities {
			lc := strings.ToLower(cap)
			card.Capabilities = append(card.Capabilities, lc)
			switch lc {
			case "tools":
				card.Tools = true
			case "vision":
				card.Vision = true
			case "thinking":
				card.Reasoning = true
			}
		}
	}
	// Specialist models are completion-capable but poor general chat
	// targets — tagged so UIs keep them out of the default chat picker.
	// The family is the reliable signal (deepseekocr, glmocr); the raw tag
	// catches the ones whose GGUF inherits a general family (olmocr2 →
	// qwen2vl).
	if strings.Contains(strings.ToLower(meta.Family), "ocr") || strings.Contains(strings.ToLower(raw), "ocr") {
		card.Specialty = "ocr"
	}
	// An effort vocabulary stated by the listing itself (TensorFold's
	// reasoning object) lands on the card without a pin — the operator's
	// --thinking-efforts still wins, applied just below.
	if len(card.ReasoningEfforts) == 0 && len(meta.Efforts) > 0 {
		card.ReasoningEfforts = meta.Efforts
	}
	if mapped {
		card.ModelID = mod.ID
		card.MaxCtx = mod.Ctx
		if opts.PriceInPer1M == 0 {
			card.PriceInPer1M = mod.SuggestedPriceInPer1M
		}
		if opts.PriceOutPer1M == 0 {
			card.PriceOutPer1M = mod.SuggestedPriceOutPer1M
		}
		if card.Family == "" {
			card.Family = mod.Family
		}
		if card.DefaultQuant == "" {
			card.DefaultQuant = mod.DefaultQuant
		}
		if card.ParamsB == 0 {
			card.ParamsB = mod.ParamsB
		}
	} else {
		card.ModelID = opts.NodeID + "/" + backend.RawTag(raw)
	}
	if card.MaxCtx == 0 {
		card.MaxCtx = meta.Ctx // measured beats catalog-for-ctx only when catalog had none
	}
	if card.PriceInPer1M == 0 && card.PriceOutPer1M == 0 && !mapped {
		card.PriceInPer1M, card.PriceOutPer1M = cat.CurvePrice(card.ParamsB)
	}
	// Who set the price: an operator override
	// beats every network default; anything the node priced itself is
	// "default" and the router may re-price it from the network list.
	if opts.PriceInPer1M != 0 || opts.PriceOutPer1M != 0 {
		card.PriceSource = "operator"
	} else {
		card.PriceSource = "default"
	}
	applyManifest(&card, opts.Manifest, raw, card.ModelID)
	opts.applyCapabilityPins(&card, raw)
	// The operator's --weights declaration (§2.2 precedence 1) pins the id
	// outright, by raw id or card id like the capability pins; the
	// fingerprint (when the snapshot is local) is the caller's job, at
	// materialize time. Startup validated the spec — a bad one here cannot
	// happen via the CLI, and an unusable value simply leaves the field to
	// the provenance tiers.
	if decl := opts.weightsDecl(raw, card.ModelID); decl != "" {
		if spec, err := weights.ParseSpec(decl); err == nil {
			card.WeightsID = spec.ID()
		}
	}
	return card
}

// weightsDecl returns the operator's --weights spec for a card ("" =
// none), matched by raw id or card id — the same lookup the capability
// pins use.
func (opts Options) weightsDecl(raw, modelID string) string {
	for _, key := range []string{raw, modelID} {
		if v, ok := opts.Weights[key]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// applyCapabilityPins forces operator-pinned capabilities on a card, by raw
// id or card id. vLLM and llama-server cannot self-report ANY capability
// (no /api/show, no supported_parameters in their model listing), so a
// backend launched with --enable-auto-tool-choice / a reasoning parser /
// a multimodal model still advertises all-false — and the router's
// chat-surface search loop gates a tools=false card out of the agentic
// path, the UI hides image upload on vision=false. The pins are the
// operator's word: they launched the backend, they know what it does.
// Cached cards go through buildCard too, so pins apply on every start,
// cache or no cache.
func (opts Options) applyCapabilityPins(card *ServedModel, raw string) {
	for _, pin := range []struct {
		models []string
		set    *bool
		cap    string
	}{
		{opts.ToolCapable, &card.Tools, "tools"},
		{opts.VisionCapable, &card.Vision, "vision"},
		{opts.ThinkingCapable, &card.Reasoning, "thinking"},
	} {
		opts.pinCapability(card, raw, pin.models, pin.set, pin.cap)
	}
	// The efforts pin replaces the vocabulary outright (it is a list, not a
	// flag): the operator who launched the backend with a known recipe
	// ("xhigh, medium, low") can declare it where probing cannot run.
	for _, key := range []string{raw, card.ModelID} {
		if v, ok := opts.EffortsCapable[key]; ok && strings.TrimSpace(v) != "" {
			efforts := []string{}
			for _, e := range strings.Split(v, ",") {
				if e = strings.TrimSpace(e); e != "" {
					efforts = append(efforts, e)
				}
			}
			if len(efforts) > 0 {
				card.ReasoningEfforts = efforts
			}
			break
		}
	}
}

func (opts Options) pinCapability(card *ServedModel, raw string, pins []string, set *bool, capName string) {
	if *set {
		return
	}
	pinned := false
	for _, p := range pins {
		if p == raw || p == card.ModelID {
			pinned = true
			break
		}
	}
	if !pinned {
		return
	}
	*set = true
	for _, c := range card.Capabilities {
		if c == capName {
			return // already listed verbatim; only the flag was missing
		}
	}
	card.Capabilities = append(card.Capabilities, capName)
}

func isNamespaced(nodeID, modelID string) bool {
	return len(modelID) > len(nodeID)+1 && modelID[:len(nodeID)+1] == nodeID+"/"
}

func setOf(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func filterByIDs(models []ServedModel, keep map[string]bool) []ServedModel {
	out := models[:0]
	for _, m := range models {
		if keep[m.ModelID] {
			out = append(out, m)
		}
	}
	return out
}

// prepareManaged is the catalog-pinned llama-server path — the one place
// the static catalog still gates (an unverified sha256 is not
// a pin, so managed stays curated).
func prepareManaged(ctx context.Context, opts Options, m *mapping.Mapping, log *slog.Logger) (*Runtime, error) {
	if len(opts.Models) != 1 {
		return nil, fmt.Errorf("managed mode serves exactly one model for now (got %d requested)", len(opts.Models))
	}
	mod, _ := m.Resolve(opts.Models[0])
	if mod == nil || mod.GGUF == nil {
		return nil, fmt.Errorf("managed model %q: no pinned GGUF in catalog", opts.Models[0])
	}

	bin, err := managed.EnsureBinary(ctx, opts.Home, log)
	if err != nil {
		return nil, err
	}
	ggufPath, err := managed.PullModel(ctx, mod.GGUF.Repo, mod.GGUF.File, mod.GGUF.SHA256, mod.GGUF.SizeBytes, opts.Home, log)
	if err != nil {
		return nil, err
	}
	srv, err := managed.Start(ctx, managed.ServerConfig{Binary: bin, GGUFPath: ggufPath, Ctx: mod.Ctx, HomeDir: opts.Home, Log: log})
	if err != nil {
		return nil, err
	}

	rt := &Runtime{
		Server:      srv,
		BackendKind: string(backend.KindOpenAICompatible),
		BackendURL:  srv.BaseURL(),
		log:         log,
		sem:         make(chan struct{}, max(opts.MaxConcurrent, 1)),
	}
	client, err := backend.New(backend.Config{Name: "managed", Kind: backend.KindOpenAICompatible, BaseURL: srv.BaseURL()})
	if err != nil {
		rt.Close()
		return nil, err
	}
	rt.Client = client

	card := ServedModel{
		ModelID:       mod.ID,
		RawID:         mod.GGUF.File,
		State:         "ready-unbenched",
		MaxCtx:        mod.Ctx,
		MaxConcurrent: max(opts.MaxConcurrent, 1),
		PriceInPer1M:  mod.SuggestedPriceInPer1M,
		PriceOutPer1M: mod.SuggestedPriceOutPer1M,
		BackendKind:   rt.BackendKind,
		BackendURL:    "local",
		BackendID:     "managed",
		Family:        mod.Family,
		ParamsB:       mod.ParamsB,
		DefaultQuant:  mod.DefaultQuant,
		SizeMB:        int(mod.GGUF.SizeBytes / (1 << 20)),
	}
	// The catalog pin verified this hash during the pull — content identity
	// for free; a manifest entry can still override it.
	card.WeightsSHA256 = strings.ToLower(mod.GGUF.SHA256)
	applyManifest(&card, opts.Manifest, card.RawID, card.ModelID)
	if opts.PriceInPer1M != 0 {
		card.PriceInPer1M = opts.PriceInPer1M
	}
	if opts.PriceOutPer1M != 0 {
		card.PriceOutPer1M = opts.PriceOutPer1M
	}
	// Same price_source contract as buildCard (see buildCard).
	if opts.PriceInPer1M != 0 || opts.PriceOutPer1M != 0 {
		card.PriceSource = "operator"
	} else {
		card.PriceSource = "default"
	}
	if !opts.SkipBench {
		// The cache applies here too: a benched managed model skips the
		// 10-minute bench on every restart unless forced (--reprobe).
		key := "managed|" + card.RawID
		cached, hit := opts.cache.Get(key)
		if hit && !opts.ForceReprobe {
			card.State = cached.State
			card.TPS = cached.TPS
			card.TTFTms = cached.TTFTms
			log.Info("bench cached — skipping", "model", card.ModelID, "probed_at", cached.ProbedAt)
		} else {
			log.Info("benchmarking", "model", card.ModelID, "raw", card.RawID)
			bctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			res, err := bench.Run(bctx, client, card.RawID)
			cancel()
			if err != nil {
				log.Warn("bench failed — advertising unbenched", "model", card.ModelID, "err", err)
			} else {
				card.State = "ready"
				card.TPS = res.TokPerSec
				card.TTFTms = res.TTFTp50ms
				log.Info("bench done", "model", card.ModelID, "ttft_ms", res.TTFTp50ms,
					"tok_s", fmt.Sprintf("%.1f", res.TokPerSec))
			}
			opts.cache.Put(key, CachedModel{
				RawID: card.RawID, Modality: wire.ModalityChat, State: card.State,
				Family: card.Family, ParamsB: card.ParamsB, Ctx: card.MaxCtx,
				SizeMB: card.SizeMB, TPS: card.TPS, TTFTms: card.TTFTms, ProbedAt: nowStamp(),
			})
			opts.cache.Save()
		}
	}
	rt.Backends = []BackendEntry{{
		ID: "managed", Kind: rt.BackendKind, URL: rt.BackendURL,
		Client: client, Models: []ServedModel{card}, sem: rt.sem, busy: &busyRing{},
	}}
	rt.Models = []ServedModel{card}
	return rt, nil
}

// Advertisement merges hardware + cards into the capability blob.
func (rt *Runtime) Advertisement(hw json.RawMessage) json.RawMessage {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	var out wire.Advertisement
	out.Hardware = hw
	out.Backend.Kind = rt.BackendKind
	out.Backend.BaseURL = "local"
	out.Backend.Managed = rt.Server != nil
	out.Models = rt.Models
	b, err := json.Marshal(out)
	if err != nil {
		return hw
	}
	return b
}

// entryFor locates the backend + card serving modelID. Empty Backends means
// a legacy hand-built Runtime (tests): everything rides Client/sem.
func (rt *Runtime) entryFor(modelID string) (*BackendEntry, *ServedModel, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if len(rt.Backends) == 0 {
		for i := range rt.Models {
			if rt.Models[i].ModelID == modelID {
				return nil, &rt.Models[i], true
			}
		}
		return nil, nil, false
	}
	for bi := range rt.Backends {
		for i := range rt.Backends[bi].Models {
			if rt.Backends[bi].Models[i].ModelID == modelID {
				return &rt.Backends[bi], &rt.Backends[bi].Models[i], true
			}
		}
	}
	return nil, nil, false
}

// EntryFor is the exported lookup (the bench command routes per card).
func (rt *Runtime) EntryFor(modelID string) (*BackendEntry, *ServedModel, bool) {
	return rt.entryFor(modelID)
}

// clientFor resolves the backend client for a card (legacy fallback).
func (rt *Runtime) clientFor(entry *BackendEntry) backend.Client {
	if entry != nil {
		return entry.Client
	}
	return rt.Client
}

// semFor resolves the admission slots for a card (legacy fallback).
func (rt *Runtime) semFor(entry *BackendEntry) chan struct{} {
	if entry != nil {
		return entry.sem
	}
	return rt.sem
}

// HandleInference serves one dispatched request: resolves the card id to
// the backend entry + raw id, streams backend deltas as wire chunks, and
// returns the node-side token accounting for the router's cross-check.
// wire.ErrBusy when that backend's concurrency slots are taken — the router
// treats it as an instant failover trigger, never a retry-here.
func (rt *Runtime) HandleInference(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	entry, card, ok := rt.entryFor(req.Model)
	if !ok {
		return wire.InferenceDoneData{}, fmt.Errorf("model %q not served by this node", req.Model)
	}
	if card.Modality == wire.ModalityEmbedding {
		return wire.InferenceDoneData{}, fmt.Errorf("model %q is an embedding model (use /v1/embeddings)", req.Model)
	}

	select {
	case rt.semFor(entry) <- struct{}{}:
		defer func() { <-rt.semFor(entry) }()
	default:
		if entry != nil && entry.busy != nil {
			entry.busy.note(time.Now()) // N4: report the refusal per backend
		}
		return wire.InferenceDoneData{}, wire.ErrBusy
	}

	// Cold-load handling: a backend may list a model it has not loaded —
	// Ollama unloads after keep_alive while the advertisement stays, and
	// the first dispatch pays the load. Announce state=loading for the
	// load's duration so the router stretches its first-token window and
	// holds the consumer instead of failing a 503 at it; back to the prior
	// state the moment the model answers (resident from its first delta
	// on) or the attempt fails. Backends that cannot report residency
	// (vLLM) never load here — Resident assumes resident for them.
	prev := card.State
	resident := rt.Resident(ctx, entry, card.RawID)
	if !resident {
		if rt.setState(card.ModelID, "loading") {
			rt.changed()
		}
		defer func() {
			if rt.setState(card.ModelID, prev) {
				rt.changed()
			}
		}()
	}
	announceResident := false
	emitChunk := func(d wire.InferenceChunkData) error {
		if !resident && !announceResident && (d.Content != "" || d.Reasoning != "" || d.FinishReason != "" || len(d.ToolCalls) > 0) {
			announceResident = true
			if rt.setState(card.ModelID, prev) {
				rt.changed()
			}
		}
		return emit(d)
	}

	msgs := make([]backend.ChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = backend.ChatMessage{Role: m.Role, Content: m.Content,
			Images: m.Images, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	}

	var done wire.InferenceDoneData
	err := rt.clientFor(entry).StreamChat(ctx, backend.ChatRequest{
		Model:          card.RawID,
		Messages:       msgs,
		MaxTokens:      req.MaxTokens,
		Tools:          req.Tools,
		ToolChoice:     req.ToolChoice,
		Reasoning:      req.Reasoning,
		Temperature:    req.Temperature,
		ResponseFormat: req.ResponseFormat,
		Priority:       req.Priority,
	}, func(d backend.Delta) error {
		if d.Usage != nil {
			done.InputTokens, done.OutputTokens = d.Usage.InputTokens, d.Usage.OutputTokens
			// Usage-log detail (usage-log-and-trends.md): cache hits and the
			// reasoning share ride the done frame when the backend reported
			// them; unreporting backends leave them 0 (stored as unknown,
			// never a zero reading).
			done.CachedTokens, done.ReasoningTokens = d.Usage.CachedTokens, d.Usage.ReasoningTokens
		}
		if d.ReasoningOutcome != nil {
			done.Reasoning = d.ReasoningOutcome
			rt.log.Info("thinking control not met", "model", card.RawID,
				"requested", d.ReasoningOutcome.Requested, "applied", d.ReasoningOutcome.Applied)
		}
		if d.Content == "" && d.Reasoning == "" && d.FinishReason == "" && len(d.ToolCalls) == 0 {
			return nil // usage-only tail deltas carry no stream payload
		}
		return emitChunk(wire.InferenceChunkData{
			Content:      d.Content,
			Reasoning:    d.Reasoning,
			FinishReason: d.FinishReason,
			ToolCalls:    d.ToolCalls,
		})
	})
	if err != nil {
		return done, err
	}
	return done, nil
}

// maxEmbedBatch mirrors the wire cap: 16 inputs per embed_request.
const maxEmbedBatch = 16

// HandleEmbed answers one embedding dispatch: resolve the card
// to an embedding-modality entry and call the backend's Embedder.
func (rt *Runtime) HandleEmbed(ctx context.Context, req wire.EmbedRequestData) (wire.EmbedDoneData, error) {
	entry, card, ok := rt.entryFor(req.Model)
	if !ok {
		return wire.EmbedDoneData{}, fmt.Errorf("model %q not served by this node", req.Model)
	}
	if card.Modality != wire.ModalityEmbedding {
		return wire.EmbedDoneData{}, fmt.Errorf("model %q is not an embedding model", req.Model)
	}
	if len(req.Input) == 0 || len(req.Input) > maxEmbedBatch {
		return wire.EmbedDoneData{}, fmt.Errorf("embed input: want 1..%d items, got %d", maxEmbedBatch, len(req.Input))
	}
	embedder, ok := rt.clientFor(entry).(backend.Embedder)
	if !ok {
		return wire.EmbedDoneData{}, fmt.Errorf("backend %q cannot embed", card.BackendID)
	}

	select {
	case rt.semFor(entry) <- struct{}{}:
		defer func() { <-rt.semFor(entry) }()
	default:
		if entry != nil && entry.busy != nil {
			entry.busy.note(time.Now()) // N4: report the refusal per backend
		}
		return wire.EmbedDoneData{}, wire.ErrBusy
	}

	vecs, usage, err := embedder.Embed(ctx, card.RawID, req.Input)
	if err != nil {
		return wire.EmbedDoneData{}, err
	}
	return wire.EmbedDoneData{Embeddings: vecs, InputTokens: usage.InputTokens}, nil
}

// ServingModelIDs lists card ids for heartbeats.
func (rt *Runtime) ServingModelIDs() []string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	ids := make([]string, 0, len(rt.Models))
	for _, m := range rt.Models {
		ids = append(ids, m.ModelID)
	}
	return ids
}

// LiveBackends is the sampler's per-backend view:
// the agent's own truth — slot capacity, held slots, ErrBusy refusals in
// the last minute, and the node-side ids each backend serves. The legacy
// single-backend runtime (hand-built test runtimes) reports nothing: its
// sem is not per backend.
func (rt *Runtime) LiveBackends() []livestats.BackendLive {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if len(rt.Backends) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]livestats.BackendLive, 0, len(rt.Backends))
	for i := range rt.Backends {
		e := &rt.Backends[i]
		models := make([]string, 0, len(e.Models))
		for _, m := range e.Models {
			models = append(models, m.ModelID)
		}
		busy := 0
		if e.busy != nil {
			busy = e.busy.count(now)
		}
		out = append(out, livestats.BackendLive{
			ID: e.ID, Kind: e.Kind, URL: e.URL,
			Models: models, Slots: cap(e.sem), Held: len(e.sem), Busy1m: busy,
		})
	}
	return out
}

// Close stops the managed server (BYO backends are not ours to stop).
func (rt *Runtime) Close() {
	if rt.Server != nil {
		rt.Server.Stop()
	}
}
