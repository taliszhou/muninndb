package llmstats

import (
	"os"
	"sync/atomic"
)

// LLMCallStats tracks aggregate LLM call metrics using lock-free atomics.
// Safe to embed in structs and access concurrently.
type LLMCallStats struct {
	TotalCalls     atomic.Int64
	TotalErrors    atomic.Int64
	TotalLatencyMs atomic.Int64
}

// Snapshot returns a point-in-time copy of the stats.
func (s *LLMCallStats) Snapshot() Snapshot {
	calls := s.TotalCalls.Load()
	errs := s.TotalErrors.Load()
	latMs := s.TotalLatencyMs.Load()
	var avgMs float64
	if calls > 0 {
		avgMs = float64(latMs) / float64(calls)
	}
	return Snapshot{Calls: calls, Errors: errs, AvgLatMs: avgMs}
}

// Snapshot is a point-in-time copy of LLMCallStats.
type Snapshot struct {
	Calls    int64
	Errors   int64
	AvgLatMs float64
}

// Provider is implemented by services that expose LLM call stats.
type Provider interface {
	LLMStats() Snapshot
}

// VerboseEnabled returns true if verbose LLM logging is enabled via
// the provided config flag or the MUNINN_LLM_VERBOSE_LOGS env var.
func VerboseEnabled(cfgFlag *bool) bool {
	if os.Getenv("MUNINN_LLM_VERBOSE_LOGS") == "true" {
		return true
	}
	return cfgFlag != nil && *cfgFlag
}

// VerboseEnabledBool is like VerboseEnabled but accepts a plain bool
// (for use with atomic.Bool.Load()).
func VerboseEnabledBool(enabled bool) bool {
	if os.Getenv("MUNINN_LLM_VERBOSE_LOGS") == "true" {
		return true
	}
	return enabled
}
