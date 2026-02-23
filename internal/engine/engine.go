package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/brief"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/autoassoc"
	enginebrief "github.com/scrypster/muninndb/internal/engine/brief"
	"github.com/scrypster/muninndb/internal/engine/coherence"
	"github.com/scrypster/muninndb/internal/engine/novelty"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/scoring"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// CognitiveForwarder is implemented by ClusterCoordinator on Lobe nodes.
// Using an interface avoids an import cycle between engine and replication.
type CognitiveForwarder interface {
	ForwardCognitiveEffects(effect mbp.CognitiveSideEffect)
}

// Engine is the cognitive database engine implementing mbp.EngineAPI.
type Engine struct {
	store            *storage.PebbleStore
	authStore        *auth.Store // nil = use Plasticity defaults (e.g. in tests)
	fts              *fts.Index
	ftsWorker        *fts.Worker  // async FTS indexing — decoupled from write hot path
	activation       *activation.ActivationEngine
	triggers         *trigger.TriggerSystem
	engramCount      atomic.Int64
	hebbianWorker    *cognitive.HebbianWorker
	decayWorker      *cognitive.DecayWorker
	contradictWorker *cognitive.Worker[cognitive.ContradictItem]
	confidenceWorker *cognitive.Worker[cognitive.ConfidenceUpdate]
	activity         *cognitive.ActivityTracker
	embedder activation.Embedder // optional embedder for embedding-based brief scoring
	// Feature subsystems (all optional, nil-safe)
	autoAssoc  *autoassoc.Worker    // write-time automatic tag-based associations
	noveltyDet  *novelty.Detector   // write-time near-duplicate detection
	noveltyJobs chan noveltyJob      // async novelty work queue
	noveltyDone chan struct{}        // signals worker shutdown
	coherence   *coherence.Registry // per-vault incremental coherence counters
	scoring     *scoring.Store      // per-vault learnable scoring weights

	// Fix 5: coherence persistence lifecycle
	coherenceFlushStop chan struct{}
	coherenceFlushDone chan struct{}

	// coordinator forwards cognitive side effects to the Cortex on Lobe nodes.
	// nil on standalone / Cortex nodes (workers handle effects locally).
	coordinator   CognitiveForwarder
	coordinatorID string // this node's ID, used as OriginNodeID in CognitiveSideEffect

	// onWrite is an optional callback invoked after every successful Write.
	// Used to notify background processors (e.g. embed retroactive worker) of new data.
	// Stored as atomic.Value to allow safe concurrent reads without a mutex.
	onWrite atomic.Value // stores func()

	// queryCounter generates fast query IDs without crypto/rand syscall overhead.
	queryCounter atomic.Uint64
	// stopOnce ensures Stop() is idempotent even if called multiple times.
	stopOnce sync.Once

	// Vault lifecycle fields
	vaultOpsMu   sync.Mutex     // guards name reservation in StartClone/StartMerge
	jobManager   *vaultjob.Manager  // tracks async clone/merge jobs
	stopCtx      context.Context    // cancelled on Stop() to signal goroutines
	stopCancel   context.CancelFunc
	hnswRegistry *hnsw.Registry     // per-vault HNSW indexes (shared with activation)
}

// SetOnWrite registers a callback invoked after every successful Write.
// Intended for wiring background processors that need to react to new data.
// Safe to call concurrently with Write.
func (e *Engine) SetOnWrite(fn func()) {
	e.onWrite.Store(fn)
}

// fastQueryID returns a unique query identifier without crypto/rand overhead.
func (e *Engine) fastQueryID() string {
	n := e.queryCounter.Add(1)
	return fmt.Sprintf("q-%016x", n)
}

// noveltyJob is the unit of work for the async novelty worker.
type noveltyJob struct {
	wsPrefix  [8]byte
	id        storage.ULID
	vaultID   uint32
	concept   string
	content   string
	vaultName string // for coherence label
}

// NewEngine creates a new Engine.
func NewEngine(
	store *storage.PebbleStore,
	authStore *auth.Store,
	ftsIdx *fts.Index,
	act *activation.ActivationEngine,
	trig *trigger.TriggerSystem,
	hebbianWorker *cognitive.HebbianWorker,
	decayWorker *cognitive.DecayWorker,
	contradictWorker *cognitive.Worker[cognitive.ContradictItem],
	confidenceWorker *cognitive.Worker[cognitive.ConfidenceUpdate],
	embedder activation.Embedder,
	hnswRegistry *hnsw.Registry,
) *Engine {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	e := &Engine{
		store:            store,
		authStore:        authStore,
		fts:              ftsIdx,
		activation:       act,
		triggers:         trig,
		hebbianWorker:    hebbianWorker,
		decayWorker:      decayWorker,
		contradictWorker: contradictWorker,
		confidenceWorker: confidenceWorker,
		activity:         cognitive.NewActivityTracker(),
		embedder:         embedder,
		autoAssoc:        autoassoc.New(store, ftsIdx),
		noveltyDet:       novelty.New(),
		noveltyJobs:      make(chan noveltyJob, 256),
		noveltyDone:      make(chan struct{}),
		coherence:        coherence.NewRegistry(),
		scoring:          scoring.NewStore(store.GetDB()),
		stopCtx:          stopCtx,
		stopCancel:       stopCancel,
		hnswRegistry:     hnswRegistry,
		jobManager:       vaultjob.NewManager(),
	}
	// Start async novelty worker to decouple O(N) Jaccard scan from write hot path.
	go e.runNoveltyWorker()
	// Start async FTS worker to decouple indexing from the write hot path.
	// The engram is already durable in Pebble before this worker runs —
	// it only controls keyword search visibility (eventual, ~100ms lag).
	if ftsIdx != nil {
		e.ftsWorker = fts.NewWorker(ftsIdx)
	}
	// Seed in-memory counter from persistent storage so Stat() is accurate after restart.
	if count, err := store.CountEngrams(context.Background()); err == nil {
		e.engramCount.Store(count)
	}
	// Backfill vault name meta keys for any vaults written before this feature existed.
	_ = store.BackfillVaultNames()

	// T1: Wire cognitive callbacks to trigger system.
	if e.hebbianWorker != nil && e.triggers != nil {
		e.hebbianWorker.OnWeightUpdate = func(ws [8]byte, id [16]byte, field string, old, new float64) {
			vaultID := wsVaultID(ws)
			e.triggers.NotifyCognitive(vaultID, storage.ULID(id), field, float32(old), float32(new))
		}
	}
	if e.decayWorker != nil && e.triggers != nil {
		e.decayWorker.OnDecayUpdate = func(ws [8]byte, id [16]byte, field string, old, new float64) {
			vaultID := wsVaultID(ws)
			e.triggers.NotifyCognitive(vaultID, storage.ULID(id), field, float32(old), float32(new))
		}
	}

	// Fix 5: Load persisted coherence counters for known vaults.
	if e.coherence != nil {
		vaultNames, _ := store.ListVaultNames()
		for _, name := range vaultNames {
			prefix := store.ResolveVaultPrefix(name)
			data, ok, err := store.ReadCoherence(prefix)
			if err != nil {
				slog.Warn("engine: failed to load coherence counters", "vault", name, "error", err)
				continue
			}
			if ok {
				e.coherence.RestoreVault(name, data)
			}
		}
		// Start periodic coherence flush goroutine.
		e.coherenceFlushStop = make(chan struct{})
		e.coherenceFlushDone = make(chan struct{})
		go e.runCoherenceFlush()
	}

	return e
}

// runCoherenceFlush periodically persists coherence counters to Pebble.
// Runs until coherenceFlushStop is closed, then does a final flush.
func (e *Engine) runCoherenceFlush() {
	defer close(e.coherenceFlushDone)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.flushCoherence()
		case <-e.coherenceFlushStop:
			e.flushCoherence() // final flush on shutdown
			return
		}
	}
}

// flushCoherence serializes all vault coherence counters to Pebble.
func (e *Engine) flushCoherence() {
	if e.coherence == nil {
		return
	}
	for name, data := range e.coherence.SerializeAll() {
		prefix := e.store.ResolveVaultPrefix(name)
		if err := e.store.WriteCoherence(prefix, data); err != nil {
			slog.Warn("engine: failed to flush coherence", "vault", name, "error", err)
		}
	}
}

// Stop gracefully shuts down all background workers.
// Idempotent: safe to call multiple times. Must be called before the process
// exits (or the Pebble DB is closed) to ensure in-flight jobs complete.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		// Cancel the engine lifecycle context to signal running goroutines (clone/merge).
		if e.stopCancel != nil {
			e.stopCancel()
		}
		if e.autoAssoc != nil {
			e.autoAssoc.Stop()
		}
		// Stop async novelty worker.
		// Do NOT close noveltyJobs — Write() guards with stopCtx.Done() but
		// closing a channel concurrently with a pending select-send is still
		// a data race detected by -race. Signal shutdown via the cancelled
		// context instead and wait for the worker to drain and exit.
		if e.noveltyDone != nil {
			<-e.noveltyDone
		}
		// Drain the FTS worker — flushes any queued indexing jobs before exit.
		if e.ftsWorker != nil {
			e.ftsWorker.Stop()
		}
		// Fix 5: Stop coherence flush goroutine and wait for final flush.
		if e.coherenceFlushStop != nil {
			close(e.coherenceFlushStop)
			<-e.coherenceFlushDone
		}
		// Stop the vault job GC goroutine.
		if e.jobManager != nil {
			e.jobManager.Close()
		}
	})
}

// SetCoordinator wires the Lobe's ClusterCoordinator so Activate() can forward
// cognitive side effects to the Cortex. nodeID is stamped as OriginNodeID in
// every CognitiveSideEffect. Call this after both Engine and ClusterCoordinator
// are constructed.
func (e *Engine) SetCoordinator(coord CognitiveForwarder, nodeID string) {
	e.coordinator = coord
	e.coordinatorID = nodeID
}

// generateEmbeddingBrief generates a brief using embedding-based sentence scoring.
// Returns a list of BriefSentences sorted by cosine similarity to the context embedding.
// Falls back to empty list if embedding fails or no sentences meet the threshold.
func (e *Engine) generateEmbeddingBrief(ctx context.Context, items []mbp.ActivationItem, contextEmbedding []float32) []mbp.BriefSentence {
	if e.embedder == nil || len(contextEmbedding) == 0 || len(items) == 0 {
		return nil
	}

	// Create the brief scorer with the embedder adapter
	adapter := newEmbedderAdapter(e.embedder)
	scorer := &brief.Scorer{
		Model:        adapter,
		Threshold:    0.72, // reasonable default for cosine similarity
		MaxSentences: 3,    // return up to 3 sentences
		MaxSentLen:   512,  // truncate long sentences
	}

	// Combine all content from activation items
	var allSentences []mbp.BriefSentence
	for _, item := range items {
		scored, err := scorer.Score(ctx, item.Content, contextEmbedding)
		if err != nil {
			continue // skip this item on error
		}

		for _, s := range scored {
			allSentences = append(allSentences, mbp.BriefSentence{
				EngramID: item.ID,
				Text:     s.Text,
				Score:    float64(s.Score),
			})
		}
	}

	// Sort by score descending and cap at MaxSentences
	// Use bubble sort for small sets
	for i := 0; i < len(allSentences)-1; i++ {
		for j := i + 1; j < len(allSentences); j++ {
			if allSentences[j].Score > allSentences[i].Score {
				allSentences[i], allSentences[j] = allSentences[j], allSentences[i]
			}
		}
	}

	maxN := 5 // return up to 5 top sentences across all engrams
	if maxN > len(allSentences) {
		maxN = len(allSentences)
	}

	if maxN > 0 {
		return allSentences[:maxN]
	}
	return nil
}

// Store returns the underlying PebbleStore, used by the MCP adapter for direct storage access.
func (e *Engine) Store() *storage.PebbleStore {
	return e.store
}

// CountEmbedded returns the count of engrams that have had embeddings generated
// (i.e. the DigestEmbed flag is set). Returns -1 on error.
func (e *Engine) CountEmbedded(ctx context.Context) int64 {
	const DigestEmbed uint8 = 0x02
	count, err := e.store.CountWithFlag(ctx, DigestEmbed)
	if err != nil {
		return -1
	}
	return count
}

// ActivityTracker returns the vault-level activity tracker.
func (e *Engine) ActivityTracker() *cognitive.ActivityTracker {
	return e.activity
}

// Hello implements mbp.EngineAPI.Hello.
func (e *Engine) Hello(ctx context.Context, req *mbp.HelloRequest) (*mbp.HelloResponse, error) {
	if req.Version != "1.0" {
		return nil, fmt.Errorf("unsupported version: %s", req.Version)
	}

	return &mbp.HelloResponse{
		ServerVersion: "1.0.0",
		SessionID:     uuid.New().String(),
		VaultID:       req.Vault,
		Capabilities:  []string{"compression"},
		Limits: mbp.Limits{
			MaxResults:   100,
			MaxHopDepth:  5,
			MaxRate:      1000,
			MaxPayloadMB: 64,
		},
	}, nil
}

// Write implements mbp.EngineAPI.Write.
func (e *Engine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
	e.activity.Record(wsPrefix)

	// Build storage.Engram from request
	eng := &storage.Engram{
		Concept:    req.Concept,
		Content:    req.Content,
		Tags:       req.Tags,
		Confidence: req.Confidence,
		Stability:  req.Stability,
		Embedding:  req.Embedding,
	}

	// Convert associations
	assocs := make([]storage.Association, len(req.Associations))
	for i, a := range req.Associations {
		targetID, err := storage.ParseULID(a.TargetID)
		if err != nil {
			return nil, fmt.Errorf("parse target id: %w", err)
		}
		assocs[i] = storage.Association{
			TargetID:      targetID,
			RelType:       storage.RelType(a.RelType),
			Weight:        a.Weight,
			Confidence:    a.Confidence,
			CreatedAt:     time.Unix(0, a.CreatedAt),
			LastActivated: a.LastActivated,
		}
	}
	eng.Associations = assocs

	// Write to store
	id, err := e.store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		return nil, fmt.Errorf("write engram: %w", err)
	}

	// Persist vault name for discovery (idempotent, cheap)
	vault := req.Vault
	if vault == "" {
		vault = "default"
	}
	_ = e.store.WriteVaultName(wsPrefix, vault)

	// Submit to async FTS worker — decoupled from write hot path.
	// Engram is already durable; FTS visibility follows within ~100ms.
	if e.ftsWorker != nil {
		e.ftsWorker.Submit(fts.IndexJob{
			WS:        wsPrefix,
			ID:        [16]byte(id),
			Concept:   eng.Concept,
			CreatedBy: eng.CreatedBy,
			Content:   eng.Content,
			Tags:      eng.Tags,
		})
	}

	// Submit to contradiction worker for post-write analysis
	if e.contradictWorker != nil {
		contraAssocs := make([]cognitive.ContradictAssoc, len(eng.Associations))
		for i, assoc := range eng.Associations {
			contraAssocs[i] = cognitive.ContradictAssoc{
				EngramID:   [16]byte(id),
				TargetID:   assoc.TargetID,
				TargetHash: hashString(assoc.TargetID.String()),
				RelType:    uint16(assoc.RelType),
			}
		}
		e.contradictWorker.Submit(cognitive.ContradictItem{
			WS:           wsPrefix,
			EngramID:     [16]byte(id),
			ConceptHash:  hashString(eng.Concept),
			Associations: contraAssocs,
			OnFound: func(ev cognitive.ContradictionEvent) {
				if e.triggers != nil {
					// Use the WS prefix from the item; vaultID routing uses prefix matching
					e.triggers.NotifyContradiction(wsVaultID(wsPrefix), storage.ULID(ev.EngramA), storage.ULID(ev.EngramB), ev.Severity, "semantic")
				}
				// Submit confidence updates for both engrams involved in the contradiction.
				// A detected contradiction lowers confidence (evidence = EvidenceContradiction ≈ 0.1).
				if e.confidenceWorker != nil {
					e.confidenceWorker.Submit(cognitive.ConfidenceUpdate{
						WS:       wsPrefix,
						EngramID: ev.EngramA,
						Evidence: cognitive.EvidenceContradiction,
						Source:   "contradiction_detected",
					})
					e.confidenceWorker.Submit(cognitive.ConfidenceUpdate{
						WS:       wsPrefix,
						EngramID: ev.EngramB,
						Evidence: cognitive.EvidenceContradiction,
						Source:   "contradiction_detected",
					})
				}
			},
		})
	}

	e.engramCount.Add(1)

	// Update coherence counters for the new engram (starts as an orphan).
	if e.coherence != nil {
		vaultName := req.Vault
		if vaultName == "" {
			vaultName = "default"
		}
		e.coherence.GetOrCreate(vaultName).RecordWrite(eng.Confidence)
	}

	// Write-time novelty detection: enqueue async — O(N) Jaccard scan runs off the hot path.
	if e.noveltyDet != nil {
		vaultName := req.Vault
		if vaultName == "" {
			vaultName = "default"
		}
		job := noveltyJob{
			wsPrefix:  wsPrefix,
			id:        id,
			vaultID:   wsVaultID(wsPrefix),
			concept:   eng.Concept,
			content:   eng.Content,
			vaultName: vaultName,
		}
		select {
		case <-e.stopCtx.Done():
			// Engine shutting down — skip novelty detection to avoid send on closed channel.
		case e.noveltyJobs <- job:
		default:
			// Queue full — drop novelty check rather than block write path.
		}
	}

	// Write-time auto-association: find engrams with overlapping tags.
	if e.autoAssoc != nil && len(eng.Tags) > 0 {
		e.autoAssoc.Enqueue(autoassoc.Job{
			WSPrefix: wsPrefix,
			NewID:    id,
			Tags:     eng.Tags,
		})
	}

	// Notify trigger system after the write is committed and counted.
	// We copy the engram so the trigger worker goroutine has safe read-only access
	// to the struct after Write() returns and the caller's stack frame is potentially
	// reused. A shallow copy is safe because the trigger worker never mutates fields.
	if e.triggers != nil {
		vaultID := wsVaultID(wsPrefix)
		engCopy := *eng // struct copy; deep-copy slices so trigger worker has independent data
		engCopy.Tags = append([]string(nil), eng.Tags...)
		engCopy.Associations = append([]storage.Association(nil), eng.Associations...)
		if eng.Embedding != nil {
			engCopy.Embedding = append([]float32(nil), eng.Embedding...)
		}
		e.triggers.NotifyWrite(vaultID, &engCopy, true)
	}

	// Notify background processors (e.g. embed worker) of the new engram.
	if fn, ok := e.onWrite.Load().(func()); ok && fn != nil {
		fn()
	}

	return &mbp.WriteResponse{
		ID:        id.String(),
		CreatedAt: time.Now().UnixNano(),
	}, nil
}

// Read implements mbp.EngineAPI.Read.
func (e *Engine) Read(ctx context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)

	id, err := storage.ParseULID(req.ID)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}

	eng, err := e.store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		return nil, fmt.Errorf("get engram: %w", err)
	}

	return &mbp.ReadResponse{
		ID:             eng.ID.String(),
		Concept:        eng.Concept,
		Content:        eng.Content,
		Confidence:     eng.Confidence,
		Relevance:      eng.Relevance,
		Stability:      eng.Stability,
		AccessCount:    eng.AccessCount,
		Tags:           eng.Tags,
		State:          uint8(eng.State),
		CreatedAt:      eng.CreatedAt.UnixNano(),
		UpdatedAt:      eng.UpdatedAt.UnixNano(),
		LastAccess:     eng.LastAccess.UnixNano(),
		Summary:        eng.Summary,
		KeyPoints:      eng.KeyPoints,
		MemoryType:     uint8(eng.MemoryType),
		Classification: eng.Classification,
	}, nil
}

// Activate implements mbp.EngineAPI.Activate.
func (e *Engine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	// Resolve per-vault Plasticity config. nil authStore means use defaults (tests, bench).
	var resolved auth.ResolvedPlasticity
	if e.authStore != nil {
		vaultCfg, err := e.authStore.GetVaultConfig(req.Vault)
		if err == nil {
			resolved = auth.ResolvePlasticity(vaultCfg.Plasticity)
		} else {
			slog.Warn("plasticity: failed to read vault config, using defaults",
				"vault", req.Vault, "err", err)
			resolved = auth.ResolvePlasticity(nil)
		}
	} else {
		resolved = auth.ResolvePlasticity(nil)
	}

	// Build activation.ActivateRequest
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
	e.activity.Record(wsPrefix)
	actReq := &activation.ActivateRequest{
		VaultPrefix:  wsPrefix,
		Context:      req.Context,
		Embedding:    req.Embedding,
		Threshold:    float64(req.Threshold),
		MaxResults:   req.MaxResults,
		HopDepth:     req.MaxHops,
		IncludeWhy:   req.IncludeWhy,
		VaultDefault: resolved.TraversalProfile,
		Profile:      req.Profile,
	}

	// Set defaults
	if actReq.MaxResults == 0 {
		actReq.MaxResults = 20
	}
	if actReq.Threshold == 0 {
		actReq.Threshold = 0.1
	}

	// Fix 2: Default to resolved HopDepth (from Plasticity preset) BFS traversal.
	// The association graph is the primary differentiator of MuninnDB — it should
	// be active by default. Order matters: apply default FIRST, then check explicit opt-out.
	if actReq.HopDepth == 0 {
		actReq.HopDepth = resolved.HopDepth
	}
	if req.DisableHops {
		actReq.HopDepth = 0
	}

	// Fix 4: Observe mode is a pure read — skip activation log side effects.
	actReq.ReadOnly = auth.ObserveFromContext(ctx)

	// Convert weights if provided; otherwise apply preset weights from Plasticity config.
	if req.Weights != nil {
		actReq.Weights = &activation.Weights{
			SemanticSimilarity: req.Weights.SemanticSimilarity,
			FullTextRelevance:  req.Weights.FullTextRelevance,
			DecayFactor:        req.Weights.DecayFactor,
			HebbianBoost:       req.Weights.HebbianBoost,
			AccessFrequency:    req.Weights.AccessFrequency,
			Recency:            req.Weights.Recency,
		}
	} else {
		actReq.Weights = &activation.Weights{
			SemanticSimilarity: float32(resolved.SemanticWeight),
			FullTextRelevance:  float32(resolved.FTSWeight),
			DecayFactor:        float32(resolved.DecayWeight),
			HebbianBoost:       float32(resolved.HebbianWeight),
			Recency:            float32(resolved.RecencyWeight),
			AccessFrequency:    0.05, // preserved from pre-Plasticity activation default
		}
	}

	// Convert filters if provided
	if len(req.Filters) > 0 {
		actReq.Filters = make([]activation.Filter, len(req.Filters))
		for i, f := range req.Filters {
			actReq.Filters[i] = activation.Filter{
				Field: f.Field,
				Op:    f.Op,
				Value: f.Value,
			}
		}
	}

	// Run activation
	result, err := e.activation.Run(ctx, actReq)
	if err != nil {
		return nil, fmt.Errorf("activation: %w", err)
	}

	// Convert result.Scored to []mbp.ActivationItem
	items := make([]mbp.ActivationItem, len(result.Activations))
	for i, scored := range result.Activations {
		items[i] = mbp.ActivationItem{
			ID:         scored.Engram.ID.String(),
			Concept:    scored.Engram.Concept,
			Content:    scored.Engram.Content,
			Score:      float32(scored.Score),
			Confidence: scored.Engram.Confidence,
			Why:        scored.Why,
			Dormant:    scored.Dormant,
		}

		// Add score components if available
		items[i].ScoreComponents = mbp.ScoreComponents{
			SemanticSimilarity: float32(scored.Components.SemanticSimilarity),
			FullTextRelevance:  float32(scored.Components.FullTextRelevance),
			DecayFactor:        float32(scored.Components.DecayFactor),
			HebbianBoost:       float32(scored.Components.HebbianBoost),
			AccessFrequency:    float32(scored.Components.AccessFrequency),
			Recency:            float32(scored.Components.Recency),
			Raw:                float32(scored.Components.Raw),
			Final:              float32(scored.Components.Final),
		}

		// Add hop path if present
		if len(scored.HopPath) > 0 {
			items[i].HopPath = make([]string, len(scored.HopPath))
			for j, hop := range scored.HopPath {
				items[i].HopPath[j] = hop.String()
			}
		}
	}

	// Submit co-activations to Hebbian worker (skipped in observe mode or when disabled by Plasticity).
	// On Lobe nodes (hebbianWorker == nil) collect refs for forwarding to Cortex instead.
	var lobeCoActivations []mbp.CoActivationRef
	if len(result.Activations) > 0 && !auth.ObserveFromContext(ctx) && resolved.HebbianEnabled {
		if e.hebbianWorker != nil {
			coActivatedEngrams := make([]cognitive.CoActivatedEngram, len(result.Activations))
			for i, scored := range result.Activations {
				coActivatedEngrams[i] = cognitive.CoActivatedEngram{
					ID:    scored.Engram.ID,
					Score: scored.Score,
				}
			}
			e.hebbianWorker.Submit(cognitive.CoActivationEvent{
				WS:      wsPrefix,
				At:      time.Now(),
				Engrams: coActivatedEngrams,
			})
		} else if e.coordinator != nil {
			lobeCoActivations = make([]mbp.CoActivationRef, len(result.Activations))
			for i, scored := range result.Activations {
				lobeCoActivations[i] = mbp.CoActivationRef{
					ID:    [16]byte(scored.Engram.ID),
					Score: scored.Score,
				}
			}
		}
	}

	// Submit decay candidates to Decay worker (skipped in observe mode or when disabled by Plasticity).
	// On Lobe nodes (decayWorker == nil) collect accessed IDs for forwarding to Cortex instead.
	// Use SubmitBatch to reduce per-item channel contention under high concurrency.
	var lobeAccessedIDs [][16]byte
	if len(result.Activations) > 0 && !auth.ObserveFromContext(ctx) && resolved.DecayEnabled {
		if e.decayWorker != nil {
			now := time.Now()
			decayItems := make([]cognitive.DecayCandidate, len(result.Activations))
			for i, scored := range result.Activations {
				decayItems[i] = cognitive.DecayCandidate{
					WS:          wsPrefix,
					ID:          scored.Engram.ID,
					LastAccess:  now,
					AccessCount: scored.Engram.AccessCount,
					Stability:   scored.Engram.Stability,
					Relevance:   scored.Engram.Relevance,
				}
			}
			e.decayWorker.SubmitBatch(decayItems)
		} else if e.coordinator != nil {
			lobeAccessedIDs = make([][16]byte, len(result.Activations))
			for i, scored := range result.Activations {
				lobeAccessedIDs[i] = [16]byte(scored.Engram.ID)
			}
		}
	}

	// Co-activation is evidence for confidence: each retrieved engram is more
	// likely to be relevant. Submit confidence boosts (skipped in observe mode).
	// Use SubmitBatch to reduce per-item channel contention under high concurrency.
	if e.confidenceWorker != nil && len(result.Activations) > 0 && !auth.ObserveFromContext(ctx) {
		confItems := make([]cognitive.ConfidenceUpdate, len(result.Activations))
		for i, scored := range result.Activations {
			confItems[i] = cognitive.ConfidenceUpdate{
				WS:       wsPrefix,
				EngramID: [16]byte(scored.Engram.ID),
				Evidence: cognitive.EvidenceCoActivation,
				Source:   "co_activation",
			}
		}
		e.confidenceWorker.SubmitBatch(confItems)
	}

	// Forward collected Lobe side effects to the Cortex asynchronously.
	// Only fires when workers are nil (Lobe mode) and coordinator is wired.
	if e.coordinator != nil && (len(lobeCoActivations) > 0 || len(lobeAccessedIDs) > 0) {
		effect := mbp.CognitiveSideEffect{
			QueryID:       e.fastQueryID(),
			OriginNodeID:  e.coordinatorID,
			Timestamp:     time.Now().UnixNano(),
			CoActivations: lobeCoActivations,
			AccessedIDs:   lobeAccessedIDs,
		}
		e.coordinator.ForwardCognitiveEffects(effect)
	}

	// Build activation brief (extractive summarization, LLM-free).
	// BriefMode: "" or "auto" → embedding-based if embedder available, else fallback to heuristic;
	//            "extractive" → always heuristic-based; "llm" → skip (LLM not wired here yet).
	var briefSentences []mbp.BriefSentence
	briefMode := req.BriefMode
	if briefMode == "" {
		briefMode = "auto"
	}
	if briefMode == "extractive" || briefMode == "auto" {
		// Try embedding-based brief if available and we have context embedding
		if briefMode == "auto" && e.embedder != nil && len(req.Embedding) > 0 {
			// Embedding-based approach: score sentences by cosine similarity to context embedding
			briefSentences = e.generateEmbeddingBrief(ctx, items, req.Embedding)
		}

		// Fallback to heuristic approach if embedding-based didn't produce results
		if len(briefSentences) == 0 {
			engContents := make([]enginebrief.EngramContent, 0, len(items))
			for _, item := range items {
				engContents = append(engContents, enginebrief.EngramContent{
					ID:      item.ID,
					Content: item.Content,
				})
			}
			sentences := enginebrief.Compute(engContents, req.Context)
			if len(sentences) > 0 {
				briefSentences = make([]mbp.BriefSentence, len(sentences))
				for i, s := range sentences {
					briefSentences[i] = mbp.BriefSentence{
						EngramID: s.EngramID,
						Text:     s.Text,
						Score:    s.Score,
					}
				}
			}
		}
	}

	return &mbp.ActivateResponse{
		QueryID:     e.fastQueryID(),
		TotalFound:  result.TotalFound,
		Activations: items,
		LatencyMs:   result.LatencyMs,
		Brief:       briefSentences,
	}, nil
}

// ActivateWithStructuredFilter is like Activate but accepts a structured filter
// (e.g., *query.Filter) that implements a Match(*storage.Engram) bool interface.
// This allows the MQL executor to pass WHERE predicates for proper post-retrieval filtering.
func (e *Engine) ActivateWithStructuredFilter(ctx context.Context, req *mbp.ActivateRequest, structuredFilter interface{}) (*mbp.ActivateResponse, error) {
	// Resolve per-vault Plasticity config. nil authStore means use defaults (tests, bench).
	var resolved auth.ResolvedPlasticity
	if e.authStore != nil {
		vaultCfg, err := e.authStore.GetVaultConfig(req.Vault)
		if err == nil {
			resolved = auth.ResolvePlasticity(vaultCfg.Plasticity)
		} else {
			slog.Warn("plasticity: failed to read vault config, using defaults",
				"vault", req.Vault, "err", err)
			resolved = auth.ResolvePlasticity(nil)
		}
	} else {
		resolved = auth.ResolvePlasticity(nil)
	}

	// Build activation.ActivateRequest (same as Activate but with StructuredFilter)
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
	e.activity.Record(wsPrefix)
	actReq := &activation.ActivateRequest{
		VaultPrefix:      wsPrefix,
		Context:          req.Context,
		Embedding:        req.Embedding,
		Threshold:        float64(req.Threshold),
		MaxResults:       req.MaxResults,
		HopDepth:         req.MaxHops,
		IncludeWhy:       req.IncludeWhy,
		VaultDefault:     resolved.TraversalProfile,
		Profile:          req.Profile,
		StructuredFilter: structuredFilter, // Pass the structured filter for post-retrieval filtering
	}

	// Set defaults
	if actReq.MaxResults == 0 {
		actReq.MaxResults = 20
	}
	if actReq.Threshold == 0 {
		actReq.Threshold = 0.1
	}

	if actReq.HopDepth == 0 {
		actReq.HopDepth = resolved.HopDepth
	}
	if req.DisableHops {
		actReq.HopDepth = 0
	}

	actReq.ReadOnly = auth.ObserveFromContext(ctx)

	// Convert weights if provided; otherwise apply preset weights from Plasticity config.
	if req.Weights != nil {
		actReq.Weights = &activation.Weights{
			SemanticSimilarity: req.Weights.SemanticSimilarity,
			FullTextRelevance:  req.Weights.FullTextRelevance,
			DecayFactor:        req.Weights.DecayFactor,
			HebbianBoost:       req.Weights.HebbianBoost,
			AccessFrequency:    req.Weights.AccessFrequency,
			Recency:            req.Weights.Recency,
		}
	} else {
		actReq.Weights = &activation.Weights{
			SemanticSimilarity: float32(resolved.SemanticWeight),
			FullTextRelevance:  float32(resolved.FTSWeight),
			DecayFactor:        float32(resolved.DecayWeight),
			HebbianBoost:       float32(resolved.HebbianWeight),
			Recency:            float32(resolved.RecencyWeight),
			AccessFrequency:    0.05, // preserved from pre-Plasticity activation default
		}
	}

	// Convert filters if provided
	if len(req.Filters) > 0 {
		actReq.Filters = make([]activation.Filter, len(req.Filters))
		for i, f := range req.Filters {
			actReq.Filters[i] = activation.Filter{
				Field: f.Field,
				Op:    f.Op,
				Value: f.Value,
			}
		}
	}

	// Run activation (will use StructuredFilter for post-retrieval filtering)
	result, err := e.activation.Run(ctx, actReq)
	if err != nil {
		return nil, fmt.Errorf("activation: %w", err)
	}

	// Convert result.Activations to []mbp.ActivationItem (same as Activate)
	items := make([]mbp.ActivationItem, len(result.Activations))
	for i, scored := range result.Activations {
		items[i] = mbp.ActivationItem{
			ID:         scored.Engram.ID.String(),
			Concept:    scored.Engram.Concept,
			Content:    scored.Engram.Content,
			Score:      float32(scored.Score),
			Confidence: scored.Engram.Confidence,
			Why:        scored.Why,
			Dormant:    scored.Dormant,
		}

		// Add score components
		items[i].ScoreComponents = mbp.ScoreComponents{
			SemanticSimilarity: float32(scored.Components.SemanticSimilarity),
			FullTextRelevance:  float32(scored.Components.FullTextRelevance),
			DecayFactor:        float32(scored.Components.DecayFactor),
			HebbianBoost:       float32(scored.Components.HebbianBoost),
			AccessFrequency:    float32(scored.Components.AccessFrequency),
			Recency:            float32(scored.Components.Recency),
			Raw:                float32(scored.Components.Raw),
			Final:              float32(scored.Components.Final),
		}

		// Add hop path if present
		if len(scored.HopPath) > 0 {
			items[i].HopPath = make([]string, len(scored.HopPath))
			for j, hop := range scored.HopPath {
				items[i].HopPath[j] = hop.String()
			}
		}
	}

	// Submit co-activations to Hebbian worker (same logic as Activate)
	var lobeCoActivations []mbp.CoActivationRef
	if len(result.Activations) > 0 && !auth.ObserveFromContext(ctx) && resolved.HebbianEnabled {
		if e.hebbianWorker != nil {
			coActivatedEngrams := make([]cognitive.CoActivatedEngram, len(result.Activations))
			for i, scored := range result.Activations {
				coActivatedEngrams[i] = cognitive.CoActivatedEngram{
					ID:    scored.Engram.ID,
					Score: scored.Score,
				}
			}
			e.hebbianWorker.Submit(cognitive.CoActivationEvent{
				WS:      wsPrefix,
				At:      time.Now(),
				Engrams: coActivatedEngrams,
			})
		} else if e.coordinator != nil {
			lobeCoActivations = make([]mbp.CoActivationRef, len(result.Activations))
			for i, scored := range result.Activations {
				lobeCoActivations[i] = mbp.CoActivationRef{
					ID:    [16]byte(scored.Engram.ID),
					Score: scored.Score,
				}
			}
		}
	}

	// Submit decay candidates (same logic as Activate)
	var lobeAccessedIDs [][16]byte
	if len(result.Activations) > 0 && !auth.ObserveFromContext(ctx) && resolved.DecayEnabled {
		if e.decayWorker != nil {
			now := time.Now()
			decayItems := make([]cognitive.DecayCandidate, len(result.Activations))
			for i, scored := range result.Activations {
				decayItems[i] = cognitive.DecayCandidate{
					WS:          wsPrefix,
					ID:          scored.Engram.ID,
					LastAccess:  now,
					AccessCount: scored.Engram.AccessCount,
					Stability:   scored.Engram.Stability,
					Relevance:   scored.Engram.Relevance,
				}
			}
			e.decayWorker.SubmitBatch(decayItems)
		} else if e.coordinator != nil {
			lobeAccessedIDs = make([][16]byte, len(result.Activations))
			for i, scored := range result.Activations {
				lobeAccessedIDs[i] = [16]byte(scored.Engram.ID)
			}
		}
	}

	// Submit confidence updates (same logic as Activate)
	if e.confidenceWorker != nil && len(result.Activations) > 0 && !auth.ObserveFromContext(ctx) {
		confItems := make([]cognitive.ConfidenceUpdate, len(result.Activations))
		for i, scored := range result.Activations {
			confItems[i] = cognitive.ConfidenceUpdate{
				WS:       wsPrefix,
				EngramID: [16]byte(scored.Engram.ID),
				Evidence: cognitive.EvidenceCoActivation,
				Source:   "co_activation",
			}
		}
		e.confidenceWorker.SubmitBatch(confItems)
	}

	// Forward collected Lobe side effects (same logic as Activate)
	if e.coordinator != nil && (len(lobeCoActivations) > 0 || len(lobeAccessedIDs) > 0) {
		effect := mbp.CognitiveSideEffect{
			QueryID:       e.fastQueryID(),
			OriginNodeID:  e.coordinatorID,
			Timestamp:     time.Now().UnixNano(),
			CoActivations: lobeCoActivations,
			AccessedIDs:   lobeAccessedIDs,
		}
		e.coordinator.ForwardCognitiveEffects(effect)
	}

	// Build activation brief (same logic as Activate)
	var briefSentences []mbp.BriefSentence
	briefMode := req.BriefMode
	if briefMode == "" {
		briefMode = "auto"
	}
	if briefMode == "extractive" || briefMode == "auto" {
		if briefMode == "auto" && e.embedder != nil && len(req.Embedding) > 0 {
			briefSentences = e.generateEmbeddingBrief(ctx, items, req.Embedding)
		}

		if len(briefSentences) == 0 {
			engContents := make([]enginebrief.EngramContent, 0, len(items))
			for _, item := range items {
				engContents = append(engContents, enginebrief.EngramContent{
					ID:      item.ID,
					Content: item.Content,
				})
			}
			sentences := enginebrief.Compute(engContents, req.Context)
			if len(sentences) > 0 {
				briefSentences = make([]mbp.BriefSentence, len(sentences))
				for j, s := range sentences {
					briefSentences[j] = mbp.BriefSentence{
						EngramID: s.EngramID,
						Text:     s.Text,
						Score:    float64(s.Score),
					}
				}
			}
		}
	}

	return &mbp.ActivateResponse{
		QueryID:     e.fastQueryID(),
		TotalFound:  result.TotalFound,
		Activations: items,
		LatencyMs:   result.LatencyMs,
		Brief:       briefSentences,
	}, nil
}

// Subscribe implements mbp.EngineAPI.Subscribe.
// Delegates to SubscribeWithDeliver with a nil deliver func (MBP/gRPC callers
// set their own deliver func after Subscribe returns via the subscription ID).
func (e *Engine) Subscribe(ctx context.Context, req *mbp.SubscribeRequest) (*mbp.SubscribeResponse, error) {
	subID, err := e.SubscribeWithDeliver(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	return &mbp.SubscribeResponse{SubID: subID, Status: "active"}, nil
}

// SubscribeWithDeliver registers a subscription and immediately sets a delivery
// function that is called (in a goroutine by DeliveryRouter) on every push.
// Returns the subscription ID. Pass deliver=nil to register without a delivery
// function (useful for MBP clients that pull via the stream).
func (e *Engine) SubscribeWithDeliver(ctx context.Context, req *mbp.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
	subID := req.SubscriptionID
	if subID == "" {
		subID = uuid.New().String()
	}

	// Resolve vault to a routing uint32 using the same BigEndian convention
	// already used in storage/impl.go (binary.BigEndian.Uint32(wsPrefix[:4])).
	// This is a compact routing key; the full 8-byte prefix is preserved in the
	// workspace prefix used for storage lookups.
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
	vaultID := wsVaultID(wsPrefix)

	sub := &trigger.Subscription{
		ID:             subID,
		VaultID:        vaultID,
		Context:        req.Context,
		Threshold:      float64(req.Threshold),
		TTL:            time.Duration(req.TTL) * time.Second,
		RateLimit:      req.RateLimit,
		PushOnWrite:    req.PushOnWrite,
		DeltaThreshold: float64(req.DeltaThreshold),
		Deliver:        deliver,
	}

	if err := e.triggers.Subscribe(sub); err != nil {
		return "", fmt.Errorf("subscribe: %w", err)
	}
	return subID, nil
}

// Unsubscribe implements mbp.EngineAPI.Unsubscribe.
func (e *Engine) Unsubscribe(ctx context.Context, subID string) error {
	e.triggers.Unsubscribe(subID)
	return nil
}

// Link implements mbp.EngineAPI.Link.
func (e *Engine) Link(ctx context.Context, req *mbp.LinkRequest) (*mbp.LinkResponse, error) {
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)

	sourceID, err := storage.ParseULID(req.SourceID)
	if err != nil {
		return nil, fmt.Errorf("parse source id: %w", err)
	}

	targetID, err := storage.ParseULID(req.TargetID)
	if err != nil {
		return nil, fmt.Errorf("parse target id: %w", err)
	}

	assoc := &storage.Association{
		TargetID:   targetID,
		RelType:    storage.RelType(req.RelType),
		Weight:     req.Weight,
		Confidence: 1.0,
		CreatedAt:  time.Now(),
	}

	if err := e.store.WriteAssociation(ctx, wsPrefix, sourceID, targetID, assoc); err != nil {
		return nil, fmt.Errorf("write association: %w", err)
	}

	// When a "contradicts" link is explicitly created via Link(), notify the
	// ContradictWorker so it can flag the pair and drive confidence updates.
	if storage.RelType(req.RelType) == storage.RelContradicts {
		if e.contradictWorker != nil {
			e.contradictWorker.Submit(cognitive.ContradictItem{
				WS:          wsPrefix,
				EngramID:    [16]byte(sourceID),
				ConceptHash: 0,
				Associations: []cognitive.ContradictAssoc{
					{
						EngramID:   [16]byte(sourceID),
						TargetID:   [16]byte(targetID),
						TargetHash: 0,
						RelType:    uint16(storage.RelContradicts),
					},
					{
						EngramID:   [16]byte(sourceID),
						TargetID:   [16]byte(sourceID),
						TargetHash: 0,
						RelType:    uint16(storage.RelSupports),
					},
				},
				OnFound: func(ev cognitive.ContradictionEvent) {
					if e.triggers != nil {
						e.triggers.NotifyContradiction(wsVaultID(wsPrefix), storage.ULID(ev.EngramA), storage.ULID(ev.EngramB), ev.Severity, "explicit_link")
					}
					// Drive confidence down for both endpoints of the contradiction.
					if e.confidenceWorker != nil {
						e.confidenceWorker.Submit(cognitive.ConfidenceUpdate{
							WS:       wsPrefix,
							EngramID: ev.EngramA,
							Evidence: cognitive.EvidenceContradiction,
							Source:   "contradiction_detected",
						})
						e.confidenceWorker.Submit(cognitive.ConfidenceUpdate{
							WS:       wsPrefix,
							EngramID: ev.EngramB,
							Evidence: cognitive.EvidenceContradiction,
							Source:   "contradiction_detected",
						})
					}
				},
			})
		}
		// Also directly flag the pair and update confidence without waiting for
		// ContradictWorker's batch processing — direct link is an explicit assertion.
		if e.confidenceWorker != nil {
			e.confidenceWorker.Submit(cognitive.ConfidenceUpdate{
				WS:       wsPrefix,
				EngramID: [16]byte(sourceID),
				Evidence: cognitive.EvidenceContradiction,
				Source:   "contradiction_detected",
			})
			e.confidenceWorker.Submit(cognitive.ConfidenceUpdate{
				WS:       wsPrefix,
				EngramID: [16]byte(targetID),
				Evidence: cognitive.EvidenceContradiction,
				Source:   "contradiction_detected",
			})
		}
	}

	return &mbp.LinkResponse{OK: true}, nil
}

// Forget implements mbp.EngineAPI.Forget.
func (e *Engine) Forget(ctx context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)

	id, err := storage.ParseULID(req.ID)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}

	if req.Hard {
		if err := e.store.DeleteEngram(ctx, wsPrefix, id); err != nil {
			return nil, fmt.Errorf("hard delete: %w", err)
		}
		// Decrement the global engram counter. Floor at zero to guard against
		// counter skew in crash-recovery scenarios (mirrors ClearVault's guard).
		for {
			cur := e.engramCount.Load()
			if cur <= 0 {
				break
			}
			if e.engramCount.CompareAndSwap(cur, cur-1) {
				break
			}
		}
	} else {
		if err := e.store.SoftDelete(ctx, wsPrefix, id); err != nil {
			return nil, fmt.Errorf("soft delete: %w", err)
		}
	}

	return &mbp.ForgetResponse{OK: true}, nil
}

// Stat implements mbp.EngineAPI.Stat.
func (e *Engine) Stat(ctx context.Context, req *mbp.StatRequest) (*mbp.StatResponse, error) {
	vaultNames, _ := e.store.ListVaultNames()
	vaultCount := len(vaultNames)
	if vaultCount == 0 {
		vaultCount = 1
	}

	resp := &mbp.StatResponse{
		EngramCount:  e.engramCount.Load(),
		VaultCount:   vaultCount,
		StorageBytes: e.store.DiskSize(),
	}

	// Attach coherence scores for all vaults if the registry is populated.
	if e.coherence != nil {
		snapshots := e.coherence.Snapshots()
		if len(snapshots) > 0 {
			resp.CoherenceScores = make(map[string]mbp.CoherenceResult, len(snapshots))
			for _, snap := range snapshots {
				resp.CoherenceScores[snap.VaultName] = mbp.CoherenceResult{
					Score:                snap.Score,
					OrphanRatio:          snap.OrphanRatio,
					ContradictionDensity: snap.ContradictionDensity,
					DuplicationPressure:  snap.DuplicationPressure,
					DecayVariance:        snap.DecayVariance,
					TotalEngrams:         snap.TotalEngrams,
				}
			}
		}
	}

	return resp, nil
}

// ListVaults returns all vault names that have been written to.
func (e *Engine) ListVaults(ctx context.Context) ([]string, error) {
	return e.store.ListVaultNames()
}

// WorkerStats returns the current statistics for all cognitive workers.
func (e *Engine) WorkerStats() cognitive.EngineWorkerStats {
	stats := cognitive.EngineWorkerStats{}
	if e.hebbianWorker != nil {
		stats.Hebbian = e.hebbianWorker.Stats()
	}
	if e.decayWorker != nil {
		stats.Decay = e.decayWorker.Stats()
	}
	if e.contradictWorker != nil {
		stats.Contradict = e.contradictWorker.Stats()
	}
	if e.confidenceWorker != nil {
		stats.Confidence = e.confidenceWorker.Stats()
	}
	return stats
}

// Restore un-deletes a soft-deleted engram by restoring its state to StateActive.
// Returns an error if the engram does not exist or was hard-deleted.
func (e *Engine) Restore(ctx context.Context, vault, id string) (*storage.Engram, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}
	eng, err := e.store.GetEngram(ctx, ws, ulid)
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	if eng.State != storage.StateSoftDeleted {
		return nil, fmt.Errorf("restore: engram %s is not soft-deleted (state=%d)", id, eng.State)
	}
	meta := &storage.EngramMeta{
		State:       storage.StateActive,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount,
		UpdatedAt:   time.Now(),
		LastAccess:  eng.LastAccess,
	}
	if err := e.store.UpdateMetadata(ctx, ws, ulid, meta); err != nil {
		return nil, fmt.Errorf("restore update: %w", err)
	}
	eng.State = storage.StateActive
	return eng, nil
}

// UpdateLifecycleState transitions an engram to the named lifecycle state.
func (e *Engine) UpdateLifecycleState(ctx context.Context, vault, id, state string) error {
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	eng, err := e.store.GetEngram(ctx, ws, ulid)
	if err != nil {
		return fmt.Errorf("get engram: %w", err)
	}
	newState, err := storage.ParseLifecycleState(state)
	if err != nil {
		return err
	}
	meta := &storage.EngramMeta{
		State:       newState,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount,
		UpdatedAt:   time.Now(),
		LastAccess:  eng.LastAccess,
	}
	return e.store.UpdateMetadata(ctx, ws, ulid, meta)
}

// ListDeleted returns soft-deleted engrams in the vault, up to limit.
func (e *Engine) ListDeleted(ctx context.Context, vault string, limit int) ([]*storage.Engram, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	ids, err := e.store.ListByState(ctx, ws, storage.StateSoftDeleted, limit)
	if err != nil {
		return nil, err
	}
	return e.store.GetEngrams(ctx, ws, ids)
}

// wsVaultID extracts a uint32 routing ID from the first 4 bytes of a workspace
// prefix. This mirrors the convention in storage/impl.go line ~146 and the
// trigger system's vaultWS() function. Used to route write events to the
// correct subscription buckets in the trigger registry.
func wsVaultID(ws [8]byte) uint32 {
	return binary.BigEndian.Uint32(ws[:4])
}

// hashString returns a simple hash of a string for concept/target matching.
func hashString(s string) uint32 {
	h := uint32(5381)
	for _, c := range s {
		h = ((h << 5) + h) + uint32(c)
	}
	return h
}

// SessionResult holds the result of a Session query.
type SessionResult struct {
	Writes []EngineSessionEntry
	Since  time.Time
}

// EngineSessionEntry represents a single write in the session window.
type EngineSessionEntry struct {
	ID      string
	Concept string
	At      time.Time
}

// Evolve creates a new version of an existing engram and soft-deletes the old one.
// It links the new engram to the old one with RelSupersedes and returns the new ID.
func (e *Engine) Evolve(ctx context.Context, vault, oldID, newContent, reason string) (storage.ULID, error) {
	old, err := e.Read(ctx, &mbp.ReadRequest{ID: oldID, Vault: vault})
	if err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: read old: %w", err)
	}

	newResp, err := e.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: old.Concept + " (evolved)",
		Content: newContent,
		Tags:    old.Tags,
	})
	if err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: write new: %w", err)
	}

	_, err = e.Link(ctx, &mbp.LinkRequest{
		SourceID: newResp.ID,
		TargetID: oldID,
		RelType:  uint16(storage.RelSupersedes),
		Weight:   1.0,
		Vault:    vault,
	})
	if err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: link: %w", err)
	}

	_, err = e.Forget(ctx, &mbp.ForgetRequest{ID: oldID, Hard: false, Vault: vault})
	if err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: forget old: %w", err)
	}

	newULID, err := storage.ParseULID(newResp.ID)
	if err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: parse new id: %w", err)
	}
	return newULID, nil
}

// Consolidate merges multiple engrams into a single new engram and archives the originals.
// Returns the new merged ID, the list of archived IDs, and any non-fatal warnings.
func (e *Engine) Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (storage.ULID, []string, []string, error) {
	if len(ids) > 50 {
		return storage.ULID{}, nil, nil, fmt.Errorf("consolidate: too many ids (max 50, got %d)", len(ids))
	}
	mergedResp, err := e.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "Consolidated memory",
		Content: mergedContent,
	})
	if err != nil {
		return storage.ULID{}, nil, nil, fmt.Errorf("consolidate: write merged: %w", err)
	}

	var archived []string
	var warnings []string
	for _, id := range ids {
		_, err := e.Forget(ctx, &mbp.ForgetRequest{ID: id, Hard: false, Vault: vault})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to archive %s: %v", id, err))
		} else {
			archived = append(archived, id)
		}
	}

	mergedULID, err := storage.ParseULID(mergedResp.ID)
	if err != nil {
		return storage.ULID{}, archived, warnings, fmt.Errorf("consolidate: parse merged id: %w", err)
	}
	return mergedULID, archived, warnings, nil
}

// Session returns all engrams written to the vault after the given time.
func (e *Engine) Session(ctx context.Context, vault string, since time.Time) (*SessionResult, error) {
	res, err := e.SessionPaged(ctx, vault, since, 0, 500)
	if err != nil {
		return nil, err
	}
	return &res.SessionResult, nil
}

// SessionPagedResult extends SessionResult with total count for pagination.
type SessionPagedResult struct {
	SessionResult
	Total int
}

// SessionPaged returns engrams created since the given time with offset/limit pagination.
func (e *Engine) SessionPaged(ctx context.Context, vault string, since time.Time, offset, limit int) (*SessionPagedResult, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	// Fetch one extra to know if there are more pages.
	engrams, err := e.store.EngramsByCreatedSince(ctx, ws, since, offset, limit)
	if err != nil {
		return nil, err
	}
	result := &SessionPagedResult{
		SessionResult: SessionResult{Since: since},
	}
	for _, eng := range engrams {
		if eng != nil {
			result.Writes = append(result.Writes, EngineSessionEntry{
				ID:      eng.ID.String(),
				Concept: eng.Concept,
				At:      eng.CreatedAt,
			})
		}
	}
	result.Total = len(result.Writes) + offset
	return result, nil
}

// Decide records an explicit decision with rationale, alternatives, and supporting evidence.
// It returns the ULID of the newly written decision engram.
func (e *Engine) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (storage.ULID, error) {
	content := rationale
	if len(alternatives) > 0 {
		content += "\n---\nAlternatives:\n" + strings.Join(alternatives, "\n")
	}

	resp, err := e.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: decision,
		Content: content,
		Tags:    []string{"decision"},
	})
	if err != nil {
		return storage.ULID{}, fmt.Errorf("decide: write: %w", err)
	}

	for _, eid := range evidenceIDs {
		_, _ = e.Link(ctx, &mbp.LinkRequest{
			SourceID: resp.ID,
			TargetID: eid,
			RelType:  uint16(storage.RelSupports),
			Weight:   1.0,
			Vault:    vault,
		})
	}

	decideULID, err := storage.ParseULID(resp.ID)
	if err != nil {
		return storage.ULID{}, fmt.Errorf("decide: parse id: %w", err)
	}
	return decideULID, nil
}

// RecordAccess increments the access count and updates the last-accessed timestamp
// for the engram identified by id in the given vault.
func (e *Engine) RecordAccess(ctx context.Context, vault, id string) error {
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return fmt.Errorf("record_access: parse id: %w", err)
	}
	eng, err := e.store.GetEngram(ctx, ws, ulid)
	if err != nil {
		return fmt.Errorf("record_access: get engram: %w", err)
	}
	meta := &storage.EngramMeta{
		State:       eng.State,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount + 1,
		UpdatedAt:   eng.UpdatedAt,
		LastAccess:  time.Now(),
	}
	return e.store.UpdateMetadata(ctx, ws, ulid, meta)
}

// runNoveltyWorker drains the noveltyJobs channel, performing O(N) Jaccard similarity
// scans and REFINES association writes entirely off the synchronous write hot path.
func (e *Engine) runNoveltyWorker() {
	defer close(e.noveltyDone)
	for {
		select {
		case <-e.stopCtx.Done():
			return
		case job, ok := <-e.noveltyJobs:
			if !ok {
				return
			}
			m := e.noveltyDet.Check(job.vaultID, job.id.String(), job.concept, job.content)
			if m == nil {
				continue
			}
			targetID, err := storage.ParseULID(m.ExistingULID)
			if err != nil {
				continue
			}
			refinesAssoc := &storage.Association{
				TargetID:   targetID,
				RelType:    storage.RelRefines,
				Weight:     float32(m.Similarity),
				Confidence: 1.0,
				CreatedAt:  time.Now(),
			}
			_ = e.store.WriteAssociation(e.stopCtx, job.wsPrefix, job.id, targetID, refinesAssoc)
			if e.coherence != nil {
				e.coherence.GetOrCreate(job.vaultName).RecordLinkCreated(true, true)
			}
		}
	}
}

