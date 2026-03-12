package embed

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/scrypster/muninndb/internal/plugin/llmstats"
)

// BatchEmbedder splits large text arrays into chunks before sending to provider.
type BatchEmbedder struct {
	provider    Provider
	limiter     *TokenBucketLimiter
	batchSize   int // max texts per request
	stats       *llmstats.LLMCallStats
	verboseLogs atomic.Bool // safe for concurrent read/write
}

// NewBatchEmbedder creates a new BatchEmbedder.
func NewBatchEmbedder(provider Provider, limiter *TokenBucketLimiter, stats *llmstats.LLMCallStats) *BatchEmbedder {
	return &BatchEmbedder{
		provider:  provider,
		limiter:   limiter,
		batchSize: provider.MaxBatchSize(),
		stats:     stats,
	}
}

// SetVerboseLogs enables or disables per-call log entries. Safe to call concurrently.
func (b *BatchEmbedder) SetVerboseLogs(flag *bool) {
	b.verboseLogs.Store(flag != nil && *flag)
}

// Embed sends texts in batches, returns concatenated embeddings.
func (b *BatchEmbedder) Embed(ctx context.Context, texts []string) ([]float32, error) {
	result := make([]float32, 0)

	for i := 0; i < len(texts); i += b.batchSize {
		end := i + b.batchSize
		if end > len(texts) {
			end = len(texts)
		}

		// Wait for rate limit token if limiter exists
		if b.limiter != nil {
			if err := b.limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}

		start := time.Now()
		chunk, err := b.provider.EmbedBatch(ctx, texts[i:end])
		latMs := time.Since(start).Milliseconds()
		if b.stats != nil {
			b.stats.TotalCalls.Add(1)
			b.stats.TotalLatencyMs.Add(latMs)
			if err != nil {
				b.stats.TotalErrors.Add(1)
			}
		}
		if llmstats.VerboseEnabledBool(b.verboseLogs.Load()) {
			attrs := []any{
				"source", "llm",
				"subsystem", "embed",
				"call_type", "embed_batch",
				"provider", b.provider.Name(),
				"latency_ms", latMs,
				"batch_size", end - i,
			}
			if err != nil {
				attrs = append(attrs, "error", err.Error())
			}
			slog.InfoContext(ctx, "llm.embed_batch", attrs...)
		}
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			return nil, fmt.Errorf("embed: provider returned no embeddings for batch")
		}
		result = append(result, chunk...)
	}

	return result, nil
}
