package plugin

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"time"
)

// pollInterval is how often the processor checks for newly written, unembedded engrams.
const pollInterval = 3 * time.Second

// maxBatchSize caps the number of engrams processed in a single pass.
// This bounds iterator lifetime during bulk imports and keeps the hot path
// responsive; any remaining unprocessed engrams are picked up on the next tick.
const maxBatchSize = 1000

// maxBackoff is the upper bound for exponential back-off when the store
// returns persistent errors on CountWithoutFlag / ScanWithoutFlag.
const maxBackoff = 5 * time.Minute

// RetroactiveProcessor processes engrams asynchronously with a registered plugin.
// It scans for engrams missing a digest flag and calls the plugin to process them.
// The processor runs continuously: it does an initial pass at startup, then polls
// every pollInterval seconds. Callers can call Notify() to wake it immediately
// (e.g. after a new engram is written) without waiting for the next poll.
type RetroactiveProcessor struct {
	store    PluginStore
	plugin   Plugin
	flagBit  uint8 // DigestEmbed or DigestEnrich
	stats    RetroactiveStats
	statsMu  sync.RWMutex
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	notifyCh chan struct{} // buffered(1); non-blocking send from Notify()
}

// NewRetroactiveProcessor creates a new processor for a plugin.
func NewRetroactiveProcessor(store PluginStore, p Plugin, flagBit uint8) *RetroactiveProcessor {
	return &RetroactiveProcessor{
		store:    store,
		plugin:   p,
		flagBit:  flagBit,
		notifyCh: make(chan struct{}, 1),
	}
}

// Notify wakes the processor to run a scan immediately, without waiting for
// the next poll tick. Safe to call concurrently; drops the signal if the
// channel is already full (i.e. a scan is already pending).
func (rp *RetroactiveProcessor) Notify() {
	select {
	case rp.notifyCh <- struct{}{}:
	default:
	}
}

// Start launches the background processing goroutine.
func (rp *RetroactiveProcessor) Start(ctx context.Context) {
	// Create a cancellable context
	ctx, rp.cancelFn = context.WithCancel(ctx)

	rp.wg.Add(1)
	go rp.run(ctx)
}

// Stop gracefully shuts down the processor.
func (rp *RetroactiveProcessor) Stop() {
	if rp.cancelFn != nil {
		rp.cancelFn()
	}
	rp.wg.Wait()
}

// Stats returns a copy of the current processor statistics.
func (rp *RetroactiveProcessor) Stats() RetroactiveStats {
	rp.statsMu.RLock()
	defer rp.statsMu.RUnlock()
	return rp.stats
}

func (rp *RetroactiveProcessor) run(ctx context.Context) {
	defer rp.wg.Done()

	rp.statsMu.Lock()
	rp.stats.PluginName = rp.plugin.Name()
	rp.stats.Status = "running"
	rp.stats.StartedAt = time.Now()
	rp.statsMu.Unlock()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var consecutiveErrors int

	// Initial pass immediately on start.
	if rp.processBatch(ctx) {
		consecutiveErrors = 0
	} else {
		consecutiveErrors++
	}

	for {
		select {
		case <-ctx.Done():
			rp.statsMu.Lock()
			rp.stats.Status = "stopped"
			rp.statsMu.Unlock()
			return
		case <-rp.notifyCh:
			if rp.processBatch(ctx) {
				consecutiveErrors = 0
			} else {
				consecutiveErrors++
				rp.backoff(ctx, consecutiveErrors)
			}
		case <-ticker.C:
			if rp.processBatch(ctx) {
				consecutiveErrors = 0
			} else {
				consecutiveErrors++
				rp.backoff(ctx, consecutiveErrors)
			}
		}
	}
}

// backoff sleeps for an exponentially increasing duration (capped at maxBackoff)
// when the store returns persistent errors, preventing log floods.
// Returns early if ctx is cancelled.
func (rp *RetroactiveProcessor) backoff(ctx context.Context, consecutiveErrors int) {
	if consecutiveErrors <= 1 {
		return // first error: no extra wait, let the normal ticker handle it
	}
	// 2^(n-1) * pollInterval, capped at maxBackoff
	wait := pollInterval * (1 << (consecutiveErrors - 1))
	if wait > maxBackoff {
		wait = maxBackoff
	}
	slog.Warn("retroactive processor: backing off due to store errors",
		"plugin", rp.plugin.Name(),
		"consecutive_errors", consecutiveErrors,
		"backoff", wait)
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// processBatch scans for unprocessed engrams and processes up to maxBatchSize
// in one pass. Returns true on success (including zero-work passes), false if
// a store-level error prevents processing (used by run() for backoff decisions).
func (rp *RetroactiveProcessor) processBatch(ctx context.Context) bool {
	total, err := rp.store.CountWithoutFlag(ctx, rp.flagBit)
	if err != nil {
		slog.Error("retroactive processor: count failed", "plugin", rp.plugin.Name(), "error", err)
		return false
	}

	if total == 0 {
		return true
	}

	slog.Info("retroactive processor: starting", "plugin", rp.plugin.Name(), "total", total)

	rp.statsMu.Lock()
	rp.stats.Total += total
	rp.statsMu.Unlock()

	iter := rp.store.ScanWithoutFlag(ctx, rp.flagBit)
	if iter == nil {
		slog.Error("retroactive processor: failed to create iterator", "plugin", rp.plugin.Name())
		return false
	}
	defer iter.Close()

	startTime := time.Now()
	batchCount := 0

	for iter.Next() {
		select {
		case <-ctx.Done():
			slog.Info("retroactive processor: cancelled mid-batch", "plugin", rp.plugin.Name())
			return true
		default:
		}

		// Cap per-pass work to bound iterator lifetime during bulk imports.
		if batchCount >= maxBatchSize {
			// Signal the notify channel so the next poll picks up the remainder
			// without waiting for the full ticker interval.
			rp.Notify()
			break
		}

		eng := iter.Engram()
		if eng == nil {
			continue
		}

		if err := rp.processEngram(ctx, eng); err != nil {
			slog.Warn("retroactive processor: failed to process engram",
				"plugin", rp.plugin.Name(),
				"engram_id", eng.ID.String(),
				"error", err)
			rp.statsMu.Lock()
			rp.stats.Errors++
			rp.statsMu.Unlock()
			continue
		}

		if err := rp.store.SetDigestFlag(ctx, eng.ID, rp.flagBit); err != nil {
			slog.Warn("retroactive processor: failed to set digest flag",
				"plugin", rp.plugin.Name(),
				"engram_id", eng.ID.String(),
				"error", err)
			rp.statsMu.Lock()
			rp.stats.Errors++
			rp.statsMu.Unlock()
			continue
		}

		rp.statsMu.Lock()
		rp.stats.Processed++
		processed := rp.stats.Processed
		rp.statsMu.Unlock()

		batchCount++

		if batchCount%100 == 0 {
			runtime.Gosched()
		}

		if processed%1000 == 0 {
			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 {
				rate := float64(processed) / elapsed
				remaining := total - processed
				etaSeconds := int64(float64(remaining) / rate)

				rp.statsMu.Lock()
				rp.stats.RatePerSec = rate
				rp.stats.ETASeconds = etaSeconds
				rp.statsMu.Unlock()

				slog.Info("retroactive processor: progress",
					"plugin", rp.plugin.Name(),
					"processed", processed,
					"total", total,
					"rate_per_sec", rate,
					"eta_seconds", etaSeconds)
			}
		}
	}

	rp.statsMu.Lock()
	rp.stats.Status = "idle"
	rp.statsMu.Unlock()

	slog.Info("retroactive processor: complete",
		"plugin", rp.plugin.Name(),
		"processed", rp.stats.Processed,
		"errors", rp.stats.Errors)

	return true
}

func (rp *RetroactiveProcessor) processEngram(ctx context.Context, eng *Engram) error {
	// Check if this is an embed plugin
	if embed, ok := rp.plugin.(EmbedPlugin); ok {
		// Call Embed with the concept and content
		text := eng.Concept + " " + eng.Content
		vec, err := embed.Embed(ctx, []string{text})
		if err != nil {
			return err
		}

		// Store the embedding
		if err := rp.store.UpdateEmbedding(ctx, eng.ID, vec); err != nil {
			return err
		}

		// Insert into HNSW index
		if err := rp.store.HNSWInsert(ctx, eng.ID, vec); err != nil {
			return err
		}

		// Auto-link by embedding
		if err := rp.store.AutoLinkByEmbedding(ctx, eng.ID, vec); err != nil {
			return err
		}

		return nil
	}

	// Check if this is an enrich plugin
	if enrich, ok := rp.plugin.(EnrichPlugin); ok {
		// Call Enrich
		result, err := enrich.Enrich(ctx, eng)
		if err != nil {
			return err
		}

		// Store the enrichment result
		if err := rp.store.UpdateDigest(ctx, eng.ID, result); err != nil {
			return err
		}

		// Upsert entities
		for _, entity := range result.Entities {
			if err := rp.store.UpsertEntity(ctx, entity); err != nil {
				// Log but don't fail the whole engram
				slog.Warn("failed to upsert entity", "error", err)
			}
		}

		// Upsert relationships
		for _, rel := range result.Relationships {
			if err := rp.store.UpsertRelationship(ctx, eng.ID, rel); err != nil {
				// Log but don't fail the whole engram
				slog.Warn("failed to upsert relationship", "error", err)
			}
		}

		return nil
	}

	return nil
}
