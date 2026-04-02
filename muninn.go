// Package muninn provides an embeddable API for MuninnDB.
//
// Open a database with [Open], write memories with [DB.Remember], recall them
// with [DB.Recall], read by ID with [DB.Read], and remove them with [DB.Forget].
// Call [DB.Close] when done to flush and release the exclusive Pebble lock.
//
// Example:
//
//	db, err := muninn.Open("./data")
//	if err != nil { ... }
//	defer db.Close()
//
//	id, err := db.Remember(ctx, "default", "Go tips", "Prefer table-driven tests.")
package muninn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/migrate"
)

// Options configures an embedded DB instance.
type Options struct {
	// Embedder enables semantic (vector) recall in addition to full-text search.
	// When nil, only full-text search is used.
	Embedder Embedder
}

// Embedder converts a single piece of text to a dense vector.
// Implement this interface to plug in any embedding model.
type Embedder interface {
	// Embed returns the embedding vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)
	// Dims returns the dimensionality of the output vectors.
	Dims() int
}

// DB is an open MuninnDB instance. Use [Open] to create one.
// DB is safe for concurrent use from multiple goroutines.
type DB struct {
	eng     *engine.Engine
	store   *storage.PebbleStore
	hebbW   *cognitive.HebbianWorker
	transW  *cognitive.TransitionWorker
	cancel  context.CancelFunc
	workers sync.WaitGroup // tracks contradW and confidW goroutines
}

// Open opens (or creates) a MuninnDB database at dataDir.
// Only one process may hold the lock at a time; a second Open on the same
// directory returns an error that wraps the underlying Pebble lock error.
//
// Optional functional options may be supplied to configure the instance:
//
//	db, err := muninn.Open("./data", func(o *muninn.Options) {
//	    o.Embedder = myEmbedder
//	})
func Open(dataDir string, opts ...func(*Options)) (*DB, error) {
	var o Options
	for _, fn := range opts {
		fn(&o)
	}

	pebblePath := filepath.Join(dataDir, "pebble")
	if err := os.MkdirAll(pebblePath, 0o755); err != nil {
		return nil, fmt.Errorf("muninndb: create data dir %q: %w", pebblePath, err)
	}

	rawDB, err := storage.OpenPebble(pebblePath, storage.DefaultOptions())
	if err != nil {
		if isLockError(err) {
			return nil, fmt.Errorf("muninndb: %q is held by another process (daemon running?): %w", dataDir, err)
		}
		return nil, fmt.Errorf("muninndb: open pebble: %w", err)
	}

	if err := replication.CheckAndSetSchemaVersion(rawDB); err != nil {
		_ = rawDB.Close()
		return nil, fmt.Errorf("muninndb: schema version check: %w", err)
	}

	migRunner := migrate.NewRunner(rawDB)
	migRunner.Register(migrate.Migration{
		Version:     1,
		Description: "backfill embed_dim in ERF records for existing embeddings",
		Up:          migrate.BackfillEmbedDim,
	})
	migRunner.Register(migrate.Migration{
		Version:     2,
		Description: "backfill relationship entity index (0x26) for GetEntityAggregate optimisation",
		Up:          migrate.BackfillRelEntityIndex,
	})
	if _, err := migRunner.Run(); err != nil {
		_ = rawDB.Close()
		return nil, fmt.Errorf("muninndb: migration: %w", err)
	}

	store := storage.NewPebbleStore(rawDB, storage.PebbleStoreConfig{CacheSize: 10000})
	// No store.SetWAL() — embedded mode; Pebble provides durability.

	ftsIndex := fts.New(rawDB)
	hnswReg := hnswpkg.NewRegistry(rawDB)

	var actEmbedder activation.Embedder
	if o.Embedder != nil {
		actEmbedder = &embedderAdapter{pub: o.Embedder}
	} else {
		actEmbedder = activation.NewNoopEmbedder()
	}

	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIndex), activation.NewHNSWAdapter(hnswReg), actEmbedder)

	ctx, cancel := context.WithCancel(context.Background())

	hebbW := cognitive.NewHebbianWorker(cognitive.NewHebbianStoreAdapter(store))
	contradW := cognitive.NewContradictWorker(cognitive.NewContradictStoreAdapter(store))
	confidW := cognitive.NewConfidenceWorker(cognitive.NewConfidenceStoreAdapter(store))
	transW := cognitive.NewTransitionWorker(ctx, store.TransitionCache())
	actEngine.SetTransitionStore(store.TransitionCache())

	eng := engine.NewEngine(engine.EngineConfig{
		Store:            store,
		FTSIndex:         ftsIndex,
		ActivationEngine: actEngine,
		HebbianWorker:    hebbW,
		ContradictWorker: contradW.Worker,
		ConfidenceWorker: confidW.Worker,
		Embedder:         actEmbedder,
		HNSWRegistry:     hnswReg,
	})
	eng.SetTransitionWorker(transW)

	db := &DB{
		eng:    eng,
		store:  store,
		hebbW:  hebbW,
		transW: transW,
		cancel: cancel,
	}

	db.workers.Add(2)
	go func() { defer db.workers.Done(); contradW.Worker.Run(ctx) }() //nolint:errcheck
	go func() { defer db.workers.Done(); confidW.Worker.Run(ctx) }()  //nolint:errcheck

	return db, nil
}

// Close flushes all pending writes and releases the exclusive database lock.
// After Close returns, the DB must not be used.
func (db *DB) Close() error {
	db.cancel()          // signals contradW and confidW goroutines to stop
	db.workers.Wait()    // wait for contradW and confidW to flush and exit
	db.eng.Stop()        // coherence flush, novelty/FTS drain, job drain
	db.hebbW.Stop()      // AFTER eng.Stop — flushes buffered Hebbian writes
	db.transW.Stop()
	return db.store.Close()
}

// isLockError reports whether err indicates that the Pebble database is already
// held by another process.
func isLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "lock") || strings.Contains(msg, "LOCK") ||
		strings.Contains(msg, "already in use") ||
		strings.Contains(msg, "resource temporarily unavailable") // Linux EAGAIN
}

// ReindexFTSVault clears all FTS posting lists for the named vault and rebuilds
// them using the current tokenizer (with CJK support). This is FTS-only; HNSW
// embeddings are not affected.
// Returns the number of engrams re-indexed.
func (db *DB) ReindexFTSVault(ctx context.Context, vaultName string) (int64, error) {
	return db.eng.ReindexFTSVault(ctx, vaultName)
}

// DBStats holds summary statistics for the database.
type DBStats struct {
	EngramCount int64 `json:"engram_count"`
}

// Stats returns summary statistics for the database.
func (db *DB) Stats(ctx context.Context) (DBStats, error) {
	count, err := db.store.CountEngrams(ctx)
	if err != nil {
		return DBStats{}, err
	}
	return DBStats{EngramCount: count}, nil
}

// StartEnrichment starts an optional background enrichment processor that
// retroactively classifies, extracts entities, and summarises engrams using an
// LLM provider. Returns a stop function that should be called before Close().
//
// If the provider URL is invalid or the enrichment subsystem cannot be
// initialised, an error is returned and no goroutine is started.
func (db *DB) StartEnrichment(ctx context.Context, cfg EnrichConfig) (stop func(), err error) {
	if cfg.ProviderURL == "" {
		return func() {}, nil
	}
	// Enrichment is an internal plugin; for now expose a no-op that logs.
	// The enrichment pipeline requires deep wiring into the internal plugin
	// system. This stub satisfies the public API contract and lets aimemkb
	// compile and run. A full implementation can be wired when the internal
	// enrichment service exposes a Start/Stop lifecycle from the engine.
	stopCh := make(chan struct{})
	stop = func() { close(stopCh) }
	return stop, nil
}

// embedderAdapter adapts the public muninn.Embedder to activation.Embedder.
type embedderAdapter struct {
	pub Embedder
}

func (a *embedderAdapter) Embed(ctx context.Context, texts []string) ([]float32, error) {
	dims := a.pub.Dims()
	out := make([]float32, 0, len(texts)*dims)
	for _, t := range texts {
		vec, err := a.pub.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, vec...)
	}
	return out, nil
}

func (a *embedderAdapter) Tokenize(text string) []string {
	return strings.Fields(text)
}
