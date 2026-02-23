package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/cognitive"
	plugincfg "github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/logging"
	"github.com/scrypster/muninndb/internal/mcp"
	"github.com/scrypster/muninndb/internal/plugin"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	enrichpkg "github.com/scrypster/muninndb/internal/plugin/enrich"
	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/scrypster/muninndb/internal/transport/rest"
	grpcpkg "github.com/scrypster/muninndb/internal/transport/grpc"
	"github.com/scrypster/muninndb/internal/ui"
	"github.com/scrypster/muninndb/internal/wal"
	webui "github.com/scrypster/muninndb/web"
)

const defaultMCPAddr = "127.0.0.1:8750"

const vaultUpgradeWarning = `
================================================================
NOTICE: Vault access is now fail-closed by default.

This server has existing data, but no vault access policy has
been configured. All vaults now require an API key unless
explicitly set to public.

To allow unauthenticated access to the default vault:

  curl -X POST http://HOST:PORT/api/admin/set-vault-config \
    -H "Content-Type: application/json" \
    -d '{"name":"default","public":true}'

Or generate an API key:

  muninn api-key create --vault default --label mykey

================================================================
`

// resolveEmbedInfo reads env vars and the saved plugin config to determine the
// active embed provider + model without side-effects (no network calls).
// Priority: env vars → plugin_config.json → local bundled → none.
func resolveEmbedInfo(cfg plugincfg.PluginConfig) rest.EmbedInfo {
	if rawURL := os.Getenv("MUNINN_OLLAMA_URL"); rawURL != "" {
		if provCfg, err := plugin.ParseProviderURL(rawURL); err == nil {
			return rest.EmbedInfo{Provider: "ollama", Model: provCfg.Model}
		}
		return rest.EmbedInfo{Provider: "ollama", Model: ""}
	}
	if os.Getenv("MUNINN_OPENAI_KEY") != "" {
		return rest.EmbedInfo{Provider: "openai", Model: "text-embedding-3-small"}
	}
	if os.Getenv("MUNINN_VOYAGE_KEY") != "" {
		return rest.EmbedInfo{Provider: "voyage", Model: "voyage-3"}
	}
	// Saved config fallback (env vars above take precedence).
	switch cfg.EmbedProvider {
	case "ollama":
		if cfg.EmbedURL != "" {
			if provCfg, err := plugin.ParseProviderURL(cfg.EmbedURL); err == nil {
				return rest.EmbedInfo{Provider: "ollama", Model: provCfg.Model}
			}
			return rest.EmbedInfo{Provider: "ollama", Model: ""}
		}
	case "openai":
		return rest.EmbedInfo{Provider: "openai", Model: "text-embedding-3-small"}
	case "voyage":
		return rest.EmbedInfo{Provider: "voyage", Model: "voyage-3"}
	case "none":
		return rest.EmbedInfo{Provider: "none", Model: ""}
	}
	// Bundled local embedder: on by default. Opt out with MUNINN_LOCAL_EMBED=0.
	if os.Getenv("MUNINN_LOCAL_EMBED") != "0" && embedpkg.LocalAvailable() {
		return rest.EmbedInfo{Provider: "local", Model: "all-MiniLM-L6-v2"}
	}
	return rest.EmbedInfo{Provider: "none", Model: ""}
}

// buildEmbedder constructs an embedder. Priority (highest → lowest):
//  1. Environment variables (MUNINN_OLLAMA_URL, MUNINN_OPENAI_KEY, MUNINN_VOYAGE_KEY)
//  2. Saved plugin_config.json (cfg parameter)
//  3. Bundled local ONNX model — enabled by default when the binary was built
//     with embedded assets. Disable with MUNINN_LOCAL_EMBED=0.
//  4. Noop
//
// Returns both the activation.Embedder (for query embedding) and the underlying
// plugin.EmbedPlugin (for the RetroactiveProcessor), or nil for the plugin if noop.
func buildEmbedder(ctx context.Context, cfg plugincfg.PluginConfig, dataDir string) (activation.Embedder, plugin.EmbedPlugin, error) {
	const (
		ollamaURL  = "MUNINN_OLLAMA_URL"
		openaiKey  = "MUNINN_OPENAI_KEY"
		voyageKey  = "MUNINN_VOYAGE_KEY"
		localEmbed = "MUNINN_LOCAL_EMBED"
	)

	tryEmbedService := func(providerURL string, pcfg plugin.PluginConfig) *embedpkg.EmbedService {
		svc, err := embedpkg.NewEmbedService(providerURL)
		if err != nil {
			slog.Warn("embedder service creation failed", "url", providerURL, "error", err)
			return nil
		}
		if err := svc.Init(ctx, pcfg); err != nil {
			slog.Warn("embedder init failed, trying next provider", "url", providerURL, "error", err)
			_ = svc.Close()
			return nil
		}
		return svc
	}

	// 1. Env var: Ollama
	if url := os.Getenv(ollamaURL); url != "" {
		slog.Info("initializing Ollama embedder", "url", url)
		if svc := tryEmbedService(url, plugin.PluginConfig{}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 1. Env var: OpenAI
	if key := os.Getenv(openaiKey); key != "" {
		slog.Info("initializing OpenAI embedder")
		if svc := tryEmbedService("openai://text-embedding-3-small", plugin.PluginConfig{APIKey: key}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 1. Env var: Voyage
	if key := os.Getenv(voyageKey); key != "" {
		slog.Info("initializing Voyage embedder")
		if svc := tryEmbedService("voyage://voyage-3", plugin.PluginConfig{APIKey: key}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 2. Saved config fallback
	if cfg.EmbedProvider != "" && cfg.EmbedProvider != "none" && cfg.EmbedProvider != "local" {
		switch cfg.EmbedProvider {
		case "ollama":
			if cfg.EmbedURL != "" {
				slog.Info("initializing Ollama embedder from saved config", "url", cfg.EmbedURL)
				if svc := tryEmbedService(cfg.EmbedURL, plugin.PluginConfig{}); svc != nil {
					return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
				}
			}
		case "openai":
			slog.Info("initializing OpenAI embedder from saved config")
			if svc := tryEmbedService("openai://text-embedding-3-small", plugin.PluginConfig{APIKey: cfg.EmbedAPIKey}); svc != nil {
				return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
			}
		case "voyage":
			slog.Info("initializing Voyage embedder from saved config")
			if svc := tryEmbedService("voyage://voyage-3", plugin.PluginConfig{APIKey: cfg.EmbedAPIKey}); svc != nil {
				return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
			}
		}
	}

	// 3. Bundled local ONNX model — on by default when embedded at build time.
	// Skip only if the user explicitly opts out (MUNINN_LOCAL_EMBED=0) or chose
	// "none" as their provider.
	if cfg.EmbedProvider != "none" && os.Getenv(localEmbed) != "0" && embedpkg.LocalAvailable() {
		slog.Info("initializing bundled local ONNX embedder", "data_dir", dataDir)
		if svc := tryEmbedService("local://all-MiniLM-L6-v2", plugin.PluginConfig{DataDir: dataDir}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
		slog.Warn("bundled local embedder init failed, falling back to noop")
	}

	// 4. Noop
	slog.Warn("no embedder configured, semantic similarity disabled")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  ⚠  No embedder configured — semantic search disabled.")
	fmt.Fprintln(os.Stderr, "     To use a cloud embedder: set MUNINN_OLLAMA_URL, MUNINN_OPENAI_KEY, or MUNINN_VOYAGE_KEY")
	fmt.Fprintln(os.Stderr, "     To disable this warning: set MUNINN_LOCAL_EMBED=0")
	fmt.Fprintln(os.Stderr, "")
	return activation.NewNoopEmbedder(), nil, nil
}

// buildEnricher constructs an EnrichService from environment variables.
// Reads MUNINN_ENRICH_URL to select provider and model. Supported schemes:
//
//	ollama://localhost:11434/llama3.2          (local, no key required)
//	openai://gpt-4o-mini                       (MUNINN_ENRICH_API_KEY required)
//	anthropic://claude-haiku-4-5-20251001      (MUNINN_ANTHROPIC_KEY or MUNINN_ENRICH_API_KEY)
//
// Returns nil without error if MUNINN_ENRICH_URL is not set — LLM enrichment
// is optional. Logs a warning on init failure so the server starts without
// enrichment rather than refusing to start.
// buildEnricher constructs an EnrichService. Priority:
//  1. MUNINN_ENRICH_URL env var
//  2. Saved plugin_config.json (cfg parameter)
//
// Returns nil (no error) if neither is set — LLM enrichment is optional.
func buildEnricher(ctx context.Context, cfg plugincfg.PluginConfig) plugin.EnrichPlugin {
	enrichURL := os.Getenv("MUNINN_ENRICH_URL")

	// Fall back to saved config if env var is not set.
	if enrichURL == "" && cfg.EnrichURL != "" {
		enrichURL = cfg.EnrichURL
	}

	if enrichURL == "" {
		slog.Info("no enrich plugin configured, LLM enrichment disabled")
		return nil
	}

	slog.Info("initializing enrich plugin", "url", enrichURL)
	svc, err := enrichpkg.NewEnrichService(enrichURL)
	if err != nil {
		slog.Warn("enrich plugin URL parse failed, LLM enrichment disabled", "err", err)
		return nil
	}

	// MUNINN_ANTHROPIC_KEY is an alias for MUNINN_ENRICH_API_KEY when using Anthropic.
	apiKey := os.Getenv("MUNINN_ENRICH_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("MUNINN_ANTHROPIC_KEY")
	}
	if apiKey == "" {
		apiKey = cfg.EnrichAPIKey // saved config fallback
	}
	if err := svc.Init(ctx, plugin.PluginConfig{APIKey: apiKey}); err != nil {
		slog.Warn("enrich plugin init failed (LLM provider may be down), LLM enrichment disabled", "err", err)
		return nil
	}

	slog.Info("enrich plugin initialized", "url", enrichURL)
	return svc
}

// parseCORSOrigins splits a comma-separated MUNINN_CORS_ORIGINS env var into a slice.
// Returns nil if the string is empty — no cross-origin access allowed.
func parseCORSOrigins(env string) []string {
	if env == "" {
		return nil
	}
	var origins []string
	for _, o := range strings.Split(env, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	return origins
}

// applyMemoryLimits sets GOMEMLIMIT and GOGC for the server process.
// GOMEMLIMIT prevents unbounded heap growth; GOGC controls GC frequency.
// Configure with MUNINN_MEM_LIMIT_GB (default 4) and MUNINN_GC_PERCENT (default 200).
func applyMemoryLimits() {
	const defaultMemGB = 4
	const defaultGCPercent = 200

	memGB := defaultMemGB
	if s := os.Getenv("MUNINN_MEM_LIMIT_GB"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			memGB = n
		}
	}

	gcPct := defaultGCPercent
	if s := os.Getenv("MUNINN_GC_PERCENT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			gcPct = n
		}
	}

	debug.SetMemoryLimit(int64(memGB) * 1024 * 1024 * 1024)
	debug.SetGCPercent(gcPct)
	slog.Info("memory limits applied",
		"mem_limit_gb", memGB,
		"gc_percent", gcPct,
	)
}

// runStartupMigrations runs all idempotent storage migrations on startup.
// It enumerates every known vault and calls MigrateBuckets for each one.
// Migration errors are non-fatal: a warning is logged and startup continues.
func runStartupMigrations(ctx context.Context, store *storage.PebbleStore) {
	names, err := store.ListVaultNames()
	if err != nil {
		slog.Warn("startup migration: failed to list vault names", "err", err)
		return
	}
	for _, name := range names {
		prefix := store.ResolveVaultPrefix(name)
		if err := store.MigrateBuckets(ctx, prefix); err != nil {
			slog.Warn("startup migration: MigrateBuckets failed", "vault", name, "err", err)
		}
	}
	slog.Info("startup migration complete", "vaults", len(names))
}

// handleClusterConn reads MBP frames from an incoming cluster TCP connection
// and dispatches them to the coordinator. Exits when the connection is closed.
func handleClusterConn(conn net.Conn, coord *replication.ClusterCoordinator) {
	defer conn.Close()
	for {
		frame, err := mbp.ReadFrame(conn)
		if err != nil {
			return // connection closed or error
		}
		fromNodeID := conn.RemoteAddr().String()
		if err := coord.HandleIncomingFrame(fromNodeID, frame.Type, frame.Payload); err != nil {
			log.Printf("[cluster] frame error from %s: %v", fromNodeID, err)
		}
	}
}

func runServer() {
	// Apply memory limits before any significant allocations.
	applyMemoryLimits()

	// Flags
	dataDir := flag.String("data", "./muninn-data", "data directory")
	mbpAddr := flag.String("mbp-addr", "127.0.0.1:8474", "MBP TCP listen address")
	restAddr := flag.String("rest-addr", "127.0.0.1:8475", "REST HTTP listen address")
	mcpAddr := flag.String("mcp-addr", defaultMCPAddr, "MCP JSON-RPC listen address")
	grpcAddr := flag.String("grpc-addr", "127.0.0.1:8477", "gRPC listen address")
	mcpToken := flag.String("mcp-token", "", "Bearer token for MCP auth (empty = no auth)")
	dev := flag.Bool("dev", false, "serve web assets from ./web directory (development mode)")
	var logLevelStr string
	flag.StringVar(&logLevelStr, "log-level", "info", "Log level: debug, info, warn, error")
	flag.Parse()

	var logLevel slog.Level
	if err := logLevel.UnmarshalText([]byte(logLevelStr)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: must be debug, info, warn, or error\n", logLevelStr)
		os.Exit(1)
	}
	// Create ring buffer — onAdd wired after uiSrv is constructed.
	ring := logging.NewRingBuffer(1000, nil)
	baseHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(logging.NewRingHandler(baseHandler, ring)))

	// Resolve web FS (embedded by default, filesystem in dev mode)
	var webFS fs.FS = webui.FS
	if *dev {
		// Try to find web directory relative to binary location first
		webDir := filepath.Join(filepath.Dir(os.Args[0]), "web")
		if _, err := os.Stat(webDir); err != nil {
			// Fallback: check current working directory
			webDir = "web"
		}
		webFS = os.DirFS(webDir)
		slog.Info("dev mode: serving web assets from filesystem", "dir", webDir)
	}

	// Open Pebble
	// Use 0700 so other local users cannot list or access the database directory.
	dbPath := filepath.Join(*dataDir, "pebble")
	if err := os.MkdirAll(dbPath, 0700); err != nil {
		slog.Error("create data dir", "err", err)
		os.Exit(1)
	}

	db, err := storage.OpenPebble(dbPath, storage.DefaultOptions())
	if err != nil {
		slog.Error("open pebble", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := replication.CheckAndSetSchemaVersion(db); err != nil {
		slog.Error("schema version check", "err", err)
		os.Exit(1)
	}

	// Load cluster config (disabled by default; enabled via muninn.yaml or cluster.yaml).
	clusterCfg, err := plugincfg.LoadClusterConfig(*dataDir)
	if err != nil {
		slog.Error("load cluster config", "err", err)
		os.Exit(1)
	}
	if err := clusterCfg.Validate(); err != nil {
		slog.Error("invalid cluster config", "err", err)
		os.Exit(1)
	}

	// Wire ClusterCoordinator when cluster mode is enabled.
	var coordinator *replication.ClusterCoordinator
	if clusterCfg.Enabled {
		repLog := replication.NewReplicationLog(db)
		applier := replication.NewApplier(db)
		epochStore, err := replication.NewEpochStore(db)
		if err != nil {
			slog.Error("create epoch store", "err", err)
			os.Exit(1)
		}
		coordinator = replication.NewClusterCoordinator(&clusterCfg, repLog, applier, epochStore)

		coordinator.OnBecameCortex = func(epoch uint64) {
			log.Printf("[cluster] node promoted to Cortex at epoch %d", epoch)
			// Phase 2: start cognitive workers here
		}
		coordinator.OnBecameLobe = func() {
			log.Printf("[cluster] node demoted to Lobe")
			// Phase 2: stop cognitive workers here
		}
	}

	authStore := auth.NewStore(db)
	secretPath := filepath.Join(*dataDir, "auth_secret")
	sessionSecret, err := auth.Bootstrap(authStore, secretPath)
	if err != nil {
		slog.Error("auth bootstrap failed", "err", err)
		os.Exit(1)
	}

	// Open MOL (Write-Ahead Log)
	walPath := filepath.Join(*dataDir, "wal")
	mol, err := wal.Open(walPath)
	if err != nil {
		slog.Error("open wal", "err", err)
		os.Exit(1)
	}
	defer mol.Close()

	// Recover from sealed segments (no-op replay for now)
	err = mol.Recover(db, func(e *wal.MOLEntry) error {
		return nil
	})
	if err != nil {
		slog.Error("recover wal", "err", err)
		os.Exit(1)
	}

	// Build storage layer
	store := storage.NewPebbleStore(db, 10000)

	// Run startup migrations before the engine is built.
	runStartupMigrations(context.Background(), store)

	// Create GroupCommitter
	gc := wal.NewGroupCommitter(mol, db)

	// Set WAL on store
	store.SetWAL(mol, gc)

	// Build indexes
	ftsIndex := fts.New(db)

	// Load saved plugin config (env vars always override these values).
	savedPluginCfg, err := plugincfg.LoadPluginConfig(*dataDir)
	if err != nil {
		slog.Warn("failed to load plugin config, using defaults", "err", err)
	}

	// Build embedder: env vars → saved config → local bundled → noop.
	initCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	embedder, embedPlugin, err := buildEmbedder(initCtx, savedPluginCfg, *dataDir)
	cancel()
	if err != nil {
		slog.Error("embedder build failed", "err", err)
		os.Exit(1)
	}

	// Determine embedder provider and model for the status endpoint.
	embedInfo := resolveEmbedInfo(savedPluginCfg)

	// Build enrich plugin (optional): env vars → saved config.
	enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 30*time.Second)
	enrichPlugin := buildEnricher(enrichCtx, savedPluginCfg)
	enrichCancel()

	// Build HNSW registry (multi-vault, lazy-loading)
	hnswRegistry := hnswpkg.NewRegistry(db)

	// Build activation engine
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIndex), activation.NewHNSWAdapter(hnswRegistry), embedder)

	// Build trigger system
	trigSystem := trigger.New(store, trigger.NewFTSAdapter(ftsIndex), trigger.NewHNSWAdapter(hnswRegistry), embedder)

	// Create cognitive workers with storage adapters
	hebbianWorkerImpl := cognitive.NewHebbianWorker(cognitive.NewHebbianStoreAdapter(store))
	decayWorkerImpl := cognitive.NewDecayWorker(cognitive.NewDecayStoreAdapter(store))
	decayWorkerImpl.Worker.EnableAdaptiveScaling()
	contradictWorkerImpl := cognitive.NewContradictWorker(cognitive.NewContradictStoreAdapter(store))
	confidenceWorkerImpl := cognitive.NewConfidenceWorker(cognitive.NewConfidenceStoreAdapter(store))

	// Build engine API - pass the full worker implementations
	eng := engine.NewEngine(store, authStore, ftsIndex, actEngine, trigSystem,
		hebbianWorkerImpl, decayWorkerImpl,
		contradictWorkerImpl.Worker, confidenceWorkerImpl.Worker,
		embedder, hnswRegistry)

	// Create wrapper for REST that handles the context
	restWrapper := rest.NewEngineWrapper(eng, hnswRegistry)

	// Build plugin registry and register active plugins.
	pluginRegistry := plugin.NewRegistry()
	if embedPlugin != nil {
		if err := pluginRegistry.Register(embedPlugin); err != nil {
			slog.Warn("failed to register embed plugin in registry", "err", err)
		}
	}
	if enrichPlugin != nil {
		if err := pluginRegistry.Register(enrichPlugin); err != nil {
			slog.Warn("failed to register enrich plugin in registry", "err", err)
		}
	}

	// Build transport servers
	mbpServer := mbp.NewServer(*mbpAddr, eng, authStore)
	corsOrigins := parseCORSOrigins(os.Getenv("MUNINN_CORS_ORIGINS"))
	restServer := rest.NewServer(*restAddr, restWrapper, authStore, sessionSecret, corsOrigins, embedInfo, pluginRegistry, *dataDir, rest.MCPInfo{
		Addr:     *mcpAddr,
		HasToken: *mcpToken != "",
	})

	// Build MCP server
	mcpAdapter := mcp.NewEngineAdapter(eng, enrichPlugin)
	mcpServer := mcp.New(*mcpAddr, mcpAdapter, *mcpToken)

	// Build gRPC server
	grpcAdapter := grpcpkg.NewEngineAdapter(eng)
	grpcServer := grpcpkg.NewServer(*grpcAddr, grpcAdapter, authStore)

	// Signal handling
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		slog.Info("shutdown signal received")
		cancel()
	}()

	// startCoordinator starts the TCP listener and coordinator.Run goroutines
	// for the given coordinator. Captures server-lifetime ctx from outer scope.
	startCoordinator := func(coord *replication.ClusterCoordinator, bindAddr string) {
		go func() {
			ln, err := net.Listen("tcp", bindAddr)
			if err != nil {
				log.Printf("[cluster] failed to listen on %s: %v", bindAddr, err)
				return
			}
			defer ln.Close()
			slog.Info("[cluster] TCP listener started", "addr", bindAddr)
			for {
				conn, err := ln.Accept()
				if err != nil {
					select {
					case <-ctx.Done():
						return
					default:
						log.Printf("[cluster] accept error: %v", err)
						time.Sleep(10 * time.Millisecond)
						continue
					}
				}
				go handleClusterConn(conn, coord)
			}
		}()
		go func() {
			if err := coord.Run(ctx); err != nil && err != context.Canceled {
				slog.Error("[cluster] coordinator exited", "err", err)
			}
		}()
	}

	// Start cluster coordinator when enabled at startup.
	if coordinator != nil {
		startCoordinator(coordinator, clusterCfg.BindAddr)
	}

	// Wire coordinator to REST server so admin cluster endpoints work.
	if coordinator != nil {
		restServer.SetCoordinator(coordinator)
	}

	// Wire coordinator factory so the admin enable endpoint can start cluster
	// at runtime (without a restart) when cluster.yaml is written via the UI/CLI.
	restServer.SetCoordinatorFactory(func(_ context.Context, cfg plugincfg.ClusterConfig) (*replication.ClusterCoordinator, error) {
		repLog := replication.NewReplicationLog(db)
		applier := replication.NewApplier(db)
		epochStore, err := replication.NewEpochStore(db)
		if err != nil {
			return nil, fmt.Errorf("create epoch store: %w", err)
		}
		coord := replication.NewClusterCoordinator(&cfg, repLog, applier, epochStore)
		coord.OnBecameCortex = func(epoch uint64) {
			log.Printf("[cluster] node promoted to Cortex at epoch %d", epoch)
		}
		coord.OnBecameLobe = func() {
			log.Printf("[cluster] node demoted to Lobe")
		}
		startCoordinator(coord, cfg.BindAddr)
		return coord, nil
	})

	// Start GroupCommitter
	go gc.Run(ctx)

	// Start trigger system event loop (must start before engines begin writing).
	trigSystem.Start(ctx)

	// Start cognitive workers.
	// HebbianWorker auto-starts its own goroutine in NewHebbianWorker; do NOT call Run again.
	go decayWorkerImpl.Worker.Run(ctx)
	go contradictWorkerImpl.Worker.Run(ctx)
	go confidenceWorkerImpl.Worker.Run(ctx)

	// Start RetroactiveProcessor if a real embedder is configured.
	// It runs continuously, picking up newly written engrams via Notify() or its poll ticker.
	var retroProcessor *plugin.RetroactiveProcessor
	if embedPlugin != nil {
		pStore := plugin.NewStoreAdapter(store, hnswRegistry)
		retroProcessor = plugin.NewRetroactiveProcessor(pStore, embedPlugin, plugin.DigestEmbed)
		retroProcessor.Start(ctx)
		// Wire engine → processor: each successful Write notifies the embed worker.
		eng.SetOnWrite(retroProcessor.Notify)
		slog.Info("retroactive embed processor started")
	}

	// Start servers
	errCh := make(chan error, 3)

	go func() {
		slog.Info("MBP server starting", "addr", *mbpAddr)
		if err := mbpServer.Serve(ctx); err != nil {
			errCh <- err
		}
	}()

	go func() {
		slog.Info("REST server starting", "addr", *restAddr)
		if err := restServer.Serve(ctx); err != nil {
			errCh <- err
		}
	}()

	go func() {
		slog.Info("gRPC server starting", "addr", *grpcAddr)
		if err := grpcServer.Serve(ctx); err != nil {
			slog.Error("gRPC server error", "err", err)
		}
	}()

	go func() {
		slog.Info("mcp listening", "addr", *mcpAddr)
		if err := mcpServer.Serve(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Start UI server
	uiSrv, err := ui.NewServer(webFS, restWrapper, restServer.Handler(), authStore, sessionSecret, ring)
	if err != nil {
		slog.Error("create ui server", "err", err)
		os.Exit(1)
	}
	// Wire broadcast callback now that uiSrv is available.
	ring.SetOnAdd(func(e logging.LogEntry) {
		data, _ := json.Marshal(map[string]any{
			"type":  "log_entry",
			"level": e.Level,
			"time":  e.Time.Format(time.RFC3339),
			"msg":   e.Msg,
			"attrs": e.Attrs,
		})
		uiSrv.Broadcast(data)
	})
	if err := uiSrv.Start(ctx, "127.0.0.1:8476"); err != nil {
		slog.Error("start ui server", "err", err)
		os.Exit(1)
	}
	slog.Info("UI server listening", "addr", "127.0.0.1:8476")

	slog.Info("vault fail-closed: unconfigured vaults require an API key; use muninn api-key create to grant access")

	// Upgrade notice: warn operators if data exists but no vault configs are set.
	// This detects the scenario where an operator upgraded from a version that
	// defaulted vaults to public, and now all vaults are locked (fail-closed).
	if authStore.AdminExists() {
		cfgs, err := authStore.ListVaultConfigs()
		if err == nil && len(cfgs) == 0 {
			fmt.Fprint(os.Stderr, vaultUpgradeWarning)
		}
	}

	slog.Info("MuninnDB started")

	select {
	case <-ctx.Done():
	case err := <-errCh:
		slog.Error("server error", "err", err)
		cancel()
	}

	slog.Info("shutting down")
	if retroProcessor != nil {
		retroProcessor.Stop()
	}
	if enrichPlugin != nil {
		if closer, ok := enrichPlugin.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	mbpServer.Shutdown(shutCtx)
	restServer.Shutdown(shutCtx)
	grpcShutCtx, grpcShutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer grpcShutCancel()
	if err := grpcServer.Shutdown(grpcShutCtx); err != nil {
		slog.Error("gRPC shutdown error", "err", err)
	}
	mcpShutCtx, mcpShutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer mcpShutCancel()
	if err := mcpServer.Shutdown(mcpShutCtx); err != nil {
		slog.Error("mcp shutdown error", "err", err)
	}
	uiShutCtx, uiShutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer uiShutCancel()
	if err := uiSrv.Stop(uiShutCtx); err != nil {
		slog.Error("ui server shutdown error", "err", err)
	}
	// Stop cluster coordinator before closing the DB (coordinator holds DB references).
	if coordinator != nil {
		if err := coordinator.Stop(); err != nil {
			slog.Error("[cluster] coordinator stop error", "err", err)
		}
	}
	// Stop cognitive workers: HebbianWorker auto-started its own goroutine and must be
	// stopped explicitly. eng.Stop() flushes the coherence counters (final flush) and
	// stops the autoAssoc worker.
	hebbianWorkerImpl.Stop()
	eng.Stop()
	gc.Stop()
	slog.Info("shutdown complete")
}
