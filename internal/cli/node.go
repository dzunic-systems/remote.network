package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"remote.network/node/identity"
	"remote.network/node/ingress"
	"remote.network/node/internal/backends"
	"remote.network/node/internal/bench"
	"remote.network/node/internal/hardware"
	"remote.network/node/internal/serve"
)

// backendFlags are shared by `node start` and `node bench`. They are the
// single-backend legacy surface: any flag set overrides the backend
// registry for this run (the registry file is not touched).
type backendFlags struct {
	url       string
	kind      string
	apiKey    string
	models    []string
	managed   bool
	cardsPath string
}

func (f *backendFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.url, "backend-url", "",
		"backend base URL (default for openai-compatible: http://localhost:11434/v1)")
	cmd.Flags().StringVar(&f.kind, "backend-kind", "openai-compatible",
		"wire kind: openai | openai-compatible | anthropic")
	cmd.Flags().StringVar(&f.apiKey, "backend-api-key", "",
		"backend API key (or env REMOTE_BACKEND_API_KEY; loopback needs none)")
	cmd.Flags().StringSliceVar(&f.models, "model", nil,
		"models to serve/bench (canonical, namespaced, or raw id, repeatable; default: all probed)")
	cmd.Flags().BoolVar(&f.managed, "managed", false,
		"zero-setup path: download + supervise llama-server with the catalog-pinned GGUF (one model)")
	cmd.Flags().StringVar(&f.cardsPath, "model-cards", "",
		"model-cards manifest path (descriptive card fields: display name, description, license, lineage; default: ~/.remote/model-cards.yaml when present)")
}

func (f *backendFlags) key() string {
	if f.apiKey != "" {
		return f.apiKey
	}
	return os.Getenv("REMOTE_BACKEND_API_KEY")
}

// changed reports whether any single-backend flag was set explicitly —
// the legacy override that wins over the registry file.
func (f *backendFlags) changed(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("backend-url") ||
		cmd.Flags().Changed("backend-kind") ||
		cmd.Flags().Changed("backend-api-key")
}

// backendSpecs resolves the run's backend list: explicit flags → one
// synthesized spec; else the persisted registry (missing file = one default
// entry, the original single-backend behavior); --managed bypasses both.
func backendSpecs(cmd *cobra.Command, f *backendFlags) ([]serve.BackendSpec, error) {
	if f.managed {
		return nil, nil
	}
	if f.changed(cmd) {
		return []serve.BackendSpec{{
			ID: "default", Kind: f.kind, URL: f.url, Key: f.key(), Models: f.models,
		}}, nil
	}
	entries, err := backends.Load(homeDir)
	if err != nil {
		return nil, err
	}
	specs := make([]serve.BackendSpec, len(entries))
	for i, e := range entries {
		specs[i] = serve.BackendSpec{ID: e.ID, Kind: e.Kind, URL: e.URL, Key: e.APIKey, Models: e.Models}
	}
	return specs, nil
}

func newNodeCommand() *cobra.Command {
	node := &cobra.Command{
		Use:   "node",
		Short: "Node agent lifecycle",
	}

	node.AddCommand(newStartCommand(), newBenchCommand(), newStatusCommand())
	return node
}

func newStartCommand() *cobra.Command {
	var (
		bf             backendFlags
		routerURLs     []string
		routerStrict   bool
		name           string
		routerKey      string
		routerRoot     string
		nodeToken      string
		skipBench      bool
		reprobe        bool
		keepWarm       string
		probeModels    []string
		refreshModels  time.Duration
		refreshResid   time.Duration
		retryBackends  time.Duration
		servingCtx     map[string]int
		toolCapable    []string
		visionCapable  []string
		thinkCapable   []string
		thinkEfforts   map[string]string
		maxConc        int
		noDirect       bool
		noReverseQUIC  bool
		simulateSymNat bool
		priceIn        int
		priceOut       int
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Detect hardware, prepare the backends, connect to a router, and serve",
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, u := range routerURLs {
				if err := validateRouterURL(u); err != nil {
					return fmt.Errorf("--router %q: %w", u, err)
				}
			}
			log := newLogger()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			priv, created, err := identity.LoadOrCreate(homePath("node.key"))
			if err != nil {
				return err
			}
			nodeID := identity.ID(priv.Public().(ed25519.PublicKey))
			if name == "" {
				name = defaultNodeName()
			}

			specs, err := backendSpecs(cmd, &bf)
			if err != nil {
				return err
			}

			log.Info("node agent starting",
				"node_id", nodeID,
				"identity_created", created,
				"home", homeDir,
				"router", strings.Join(routerURLs, ","),
				"router_strict", routerStrict,
				"name", name,
				"agent_version", Version,
				"backends", len(specs),
			)

			hw := hardware.Detect(homeDir)
			summary(log, hw)

			rt, err := serve.Prepare(ctx, serve.Options{
				Home:                homeDir,
				NodeID:              nodeID,
				Backends:            specs,
				Models:              bf.models,
				Managed:             bf.managed,
				SkipBench:           skipBench,
				ForceReprobe:        reprobe,
				ProbeModels:         probeModels,
				RefreshEvery:        refreshModels,
				AdmitRetryEvery:     retryBackends,
				KeepWarm:            keepWarm,
				ServingCtxOverrides: servingCtx,
				ToolCapable:         toolCapable,
				VisionCapable:       visionCapable,
				ThinkingCapable:     thinkCapable,
				EffortsCapable:      thinkEfforts,
				MaxConcurrent:       maxConc,
				PriceInPer1M:        priceIn,
				PriceOutPer1M:       priceOut,
				CardsManifest:       bf.cardsPath,
				Log:                 log,
			})
			if err != nil {
				return err
			}
			defer rt.Close()
			log.Info("serving", "models", strings.Join(rt.ServingModelIDs(), ", "))

			hwJSON, _ := json.Marshal(hw)
			runCtx, stopRun := context.WithCancel(context.Background())
			defer stopRun()

			fleet := ingress.NewFleet(ingress.FleetConfig{
				Seeds:  routerURLs,
				Strict: routerStrict,
				Base: ingress.Config{
					Identity:     priv,
					Name:         name,
					AgentVersion: Version,
					HomeDir:      homeDir,
					RouterKey:    routerKey,
					RouterRoot:   routerRoot,
					NodeToken:    nodeToken,
					Log:          log,
					OnReprobe: func(all bool, models []string) bool {
						return rt.ForceReprobe(runCtx, all, models)
					},
					Capability: func() (json.RawMessage, error) {
						return rt.Advertisement(hwJSON), nil
					},
					Handler:           rt, // serve.Runtime answers inference + embedding dispatches
					Direct:            !noDirect,
					NoReverseQUIC:     noReverseQUIC,
					SimulateSymmetric: simulateSymNat,
				},
				Log: log,
			})
			fleet.SetServingModels(rt.ServingModelIDs())

			// Keep-warm: load the pinned models once, in the background —
			// the cold-load window becomes a startup event instead of a
			// first-request failure. State flips ride SendCapabilityUpdate —
			// and so does a late backend admission (--retry-backends), which
			// must also move the heartbeat's serving list.
			rt.OnChange(func() {
				fleet.SetServingModels(rt.ServingModelIDs())
				fleet.SendCapabilityUpdate()
			})
			go rt.Warm(runCtx)

			// Residency refresh: what the backend actually has in memory
			// (Ollama /api/ps) and the context window it is truly serving
			// with — the advertisement's honest ctx.
			if refreshResid > 0 {
				go func() {
					t := time.NewTicker(refreshResid)
					defer t.Stop()
					for {
						select {
						case <-runCtx.Done():
							return
						case <-t.C:
							resCtx, cancel := context.WithTimeout(runCtx, 30*time.Second)
							rt.ResidencyRefresh(resCtx)
							cancel()
						}
					}
				}()
			}

			// Periodic re-list: models pulled on the backend appear (and
			// removed ones drop) without an agent restart. Only NEW models
			// are characterized — cached ones never re-probe/re-bench.
			if refreshModels > 0 {
				go func() {
					t := time.NewTicker(refreshModels)
					defer t.Stop()
					for {
						select {
						case <-runCtx.Done():
							return
						case <-t.C:
							refreshCtx, cancel := context.WithTimeout(runCtx, 5*time.Minute)
							if rt.Refresh(refreshCtx) {
								if fleet.SendCapabilityUpdate() {
									log.Info("model set changed — capability update sent")
								}
							}
							cancel()
						}
					}
				}()
			}

			go func() {
				// Run retries transient session failures internally, so an
				// error here is terminal — a router key pin mismatch above
				// all. Swallowing it left the agent serving nothing while
				// looking alive.
				if err := fleet.Run(runCtx); err != nil {
					hint := "pin mismatch? check 'remote node status'; re-pin with --router-key <base32> or remove the stale ~/.remote/router_key.pin"
					if errors.Is(err, ingress.ErrNodeClaim) {
						hint = "the --node-token was refused (invalid, revoked, or the node is claimed by another account) — mint a fresh one on the dashboard or drop the flag"
					}
					log.Error("router session terminated",
						"err", err,
						"hint", hint)
				}
			}()
			defer stopRun()

			<-ctx.Done() // first signal (or parent cancel)
			log.Info("shutting down — draining in-flight requests")
			drainCtx, hardStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer hardStop()
			fleet.Drain(drainCtx, 60*time.Second)
			stopRun()
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&routerURLs, "router", []string{"ws://localhost:8080/node/v1/connect"},
		"router control endpoint(s), repeatable — the seed set; certified routers the seeds advertise are attached automatically (see --router-strict)")
	cmd.Flags().BoolVar(&routerStrict, "router-strict", false,
		"never attach routers beyond the explicit --router list (skip directory discovery)")
	cmd.Flags().StringVar(&name, "name", "", "node display name (default: hostname)")
	cmd.Flags().StringVar(&routerKey, "router-key", "",
		"pin the router's base32 public key (skip TOFU)")
	cmd.Flags().StringVar(&routerRoot, "router-root", "",
		"pin the company root public key (base32) — certified routers chaining to it are trusted, so router key rotation needs no node action; supersets --router-key when a router serves a certificate")
	cmd.Flags().StringVar(&nodeToken, "node-token", "",
		"claim this node for your dashboard account (rn_nt_… — mint one on the Nodes tab)")
	cmd.Flags().BoolVar(&skipBench, "skip-bench", false,
		"skip startup benchmark (advertises ready-unbenched)")
	cmd.Flags().BoolVar(&reprobe, "reprobe", false,
		"re-probe + re-bench every model, ignoring the probe cache")
	cmd.Flags().StringVar(&keepWarm, "keep-warm", "pinned",
		"load pinned models into backend memory at startup, resident until the backend exits: pinned (the --model list / managed model — default), all, none")
	cmd.Flags().StringSliceVar(&probeModels, "probe-model", nil,
		"raw model ids to (re)probe this run, bypassing the cache (repeatable)")
	cmd.Flags().DurationVar(&refreshModels, "refresh-models", 10*time.Minute,
		"re-list backends this often and auto-claim new/removed models (0 = off)")
	cmd.Flags().DurationVar(&refreshResid, "refresh-residency", 30*time.Second,
		"re-read backend model residency this often (Ollama /api/ps): serving ctx + loaded/cold — 0 = off")
	cmd.Flags().DurationVar(&retryBackends, "retry-backends", 2*time.Minute,
		"re-probe backends that failed at startup this often and serve them once they answer (0 = off) — start/stop backend services independently of the agent")
	cmd.Flags().StringToIntVar(&servingCtx, "serving-ctx", nil,
		"pin a model's serving context window, raw-or-card-id=tokens (repeatable; wins over what the backend reports: vLLM max_model_len, ds4-server context_length, llama.cpp /props — for backends that report nothing, or to cap the window)")
	cmd.Flags().StringSliceVar(&toolCapable, "tool-capable", nil,
		"force the tools capability on a model, raw-or-card-id (repeatable/comma-separated — vLLM and llama-server cannot self-report it; pass every model launched with tool-call support)")
	cmd.Flags().StringSliceVar(&visionCapable, "vision-capable", nil,
		"force the vision capability on a model, raw-or-card-id (repeatable — for multimodal models on backends that cannot self-report)")
	cmd.Flags().StringSliceVar(&thinkCapable, "thinking-capable", nil,
		"force the thinking capability on a model, raw-or-card-id (repeatable — for reasoning models on backends that cannot self-report)")
	cmd.Flags().StringToStringVar(&thinkEfforts, "thinking-efforts", nil,
		"pin a model's accepted reasoning_effort vocabulary, raw-or-card-id=low,medium,xhigh (the probe discovers it; use this where probing cannot run or you know the recipe — consumers pick from this list instead of guessing)")
	cmd.Flags().IntVar(&maxConc, "max-concurrent", 1,
		"max concurrent inference requests this node accepts")
	cmd.Flags().BoolVar(&noDirect, "no-direct", false,
		"never upgrade to the hole-punched direct QUIC path (stay on the WSS relay)")
	cmd.Flags().BoolVar(&noReverseQUIC, "no-reverse-quic", false,
		"never dial the router's public QUIC endpoint for the node-dialed data plane (symmetric-NAT nodes use it to leave the relay)")
	cmd.Flags().BoolVar(&simulateSymNat, "simulate-symmetric-nat", false,
		"test hook: report a symmetric NAT so the router keeps this node on the relay")
	cmd.Flags().IntVar(&priceIn, "price-in-per-1m", 0,
		"override input price for this node's cards (micro-USD per 1M tokens; 0 = catalog/curve default)")
	cmd.Flags().IntVar(&priceOut, "price-out-per-1m", 0,
		"override output price for this node's cards (micro-USD per 1M tokens; 0 = catalog/curve default)")
	bf.register(cmd)
	return cmd
}

func newBenchCommand() *cobra.Command {
	var bf backendFlags
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Probe the backends and print measured TTFT / tok/s per served model",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := newLogger()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			specs, err := backendSpecs(cmd, &bf)
			if err != nil {
				return err
			}
			rt, err := serve.Prepare(ctx, serve.Options{
				Home:          homeDir,
				NodeID:        "bench", // display namespace only; bench never connects
				Backends:      specs,
				Models:        bf.models,
				Managed:       bf.managed,
				SkipBench:     true,
				CardsManifest: bf.cardsPath,
				Log:           log,
			})
			if err != nil {
				return err
			}
			defer rt.Close()

			fmt.Println("backends:", len(rt.Backends), "cards:", len(rt.Models))
			wantAll := len(bf.models) == 0
			for _, card := range rt.Models {
				if !wantAll {
					match := false
					for _, req := range bf.models {
						if req == card.ModelID || req == card.RawID {
							match = true
						}
					}
					if !match {
						continue
					}
				}
				entry, _, _ := rt.EntryFor(card.ModelID)
				client := rt.Client
				if entry != nil {
					client = entry.Client
				}
				fmt.Println("benching", card.RawID, "→", card.ModelID,
					"("+card.Modality+", "+fmt.Sprintf("%.1fB", card.ParamsB)+")")
				if card.Modality == "embedding" {
					fmt.Println("  (embedding model — no chat bench)")
					continue
				}
				res, err := bench.Run(ctx, client, card.RawID)
				if err != nil {
					fmt.Println("  failed:", err)
					continue
				}
				fmt.Println(" ", res.String())
			}
			return nil
		},
	}
	bf.register(cmd)
	return cmd
}

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print local identity, router pin, and last known state",
		RunE: func(cmd *cobra.Command, args []string) error {
			priv, _, err := identity.LoadOrCreate(homePath("node.key"))
			if err != nil {
				return err
			}
			fmt.Printf("node_id: %s\n", identity.ID(priv.Public().(ed25519.PublicKey)))
			fmt.Printf("home:    %s\n", homeDir)
			if b, err := os.ReadFile(homePath("router_key.pin")); err == nil {
				fmt.Printf("router pin: %s\n", string(b))
			} else {
				fmt.Println("router pin: (none — will pin on first contact)")
			}
			if b, err := os.ReadFile(homePath("state.json")); err == nil {
				fmt.Println(string(b))
			} else {
				fmt.Println("state: (never connected)")
			}
			return nil
		},
	}
}

// validateRouterURL fails fast on the mangled --router shapes that would
// otherwise surface as an endless cryptic DNS retry loop: gorilla dials the
// URL's authority verbatim, so `ws://http://…` parses with host "http" and
// the agent asks DNS to resolve the literal name "http" every backoff tick.
func validateRouterURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("scheme must be ws:// or wss://, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("missing host")
	}
	// A slash before :port pushes it into the path: ws://host/:38080/…
	// dials port 80 with path /:38080/… (observed in the field as a
	// "bad handshake" loop against a 301-ing nginx on port 80).
	if strings.HasPrefix(u.Path, "/:") {
		return errors.New("port landed in the path — no slash before :port (want ws(s)://<host>:<port>/node/v1/connect)")
	}
	// Trim wss first — trimming "ws://" off "wss://…" would leave "s://…"
	// and false-positive the double-scheme check.
	rest := strings.TrimPrefix(strings.TrimPrefix(raw, "wss://"), "ws://")
	if strings.Contains(rest, "://") {
		return errors.New("double scheme — drop the inner http(s):// (want ws(s)://<host>:<port>/node/v1/connect)")
	}
	return nil
}

func defaultNodeName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unnamed-node"
	}
	return h
}

// summary logs a compact human-readable hardware digest at connect time.
func summary(log *slog.Logger, c *hardware.Capability) {
	gpuDesc := "none (CPU inference)"
	if len(c.GPUs) > 0 {
		g := c.GPUs[0]
		gpuDesc = fmt.Sprintf("%s (%s, %d MB", g.Name, g.Vendor, g.MemoryMB)
		if g.MetalFamily != "" {
			gpuDesc += ", Metal " + g.MetalFamily
		}
		gpuDesc += ")"
	}
	log.Info("hardware detected",
		"cpu", fmt.Sprintf("%s ×%d", c.CPUModel, c.CPUCores),
		"ram_mb", c.TotalMemoryMB,
		"gpu", gpuDesc,
		"disk_available_mb", c.AvailableDiskMB,
		"unified_memory", c.UnifiedMemory,
	)
}
