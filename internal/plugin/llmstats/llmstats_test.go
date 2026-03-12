package llmstats_test

import (
	"testing"

	"github.com/scrypster/muninndb/internal/plugin/llmstats"
)

func TestSnapshot_ZeroCalls(t *testing.T) {
	var s llmstats.LLMCallStats
	snap := s.Snapshot()
	if snap.Calls != 0 || snap.Errors != 0 || snap.AvgLatMs != 0 {
		t.Errorf("expected zero snapshot, got %+v", snap)
	}
}

func TestSnapshot_AvgLatency(t *testing.T) {
	var s llmstats.LLMCallStats
	s.TotalCalls.Add(4)
	s.TotalLatencyMs.Add(400)
	snap := s.Snapshot()
	if snap.AvgLatMs != 100.0 {
		t.Errorf("expected avg 100ms, got %f", snap.AvgLatMs)
	}
	if snap.Calls != 4 {
		t.Errorf("expected 4 calls, got %d", snap.Calls)
	}
}

func TestSnapshot_ErrorCount(t *testing.T) {
	var s llmstats.LLMCallStats
	s.TotalCalls.Add(10)
	s.TotalErrors.Add(3)
	s.TotalLatencyMs.Add(1000)
	snap := s.Snapshot()
	if snap.Errors != 3 {
		t.Errorf("expected 3 errors, got %d", snap.Errors)
	}
}

func TestVerboseEnabled_NilFlag(t *testing.T) {
	t.Setenv("MUNINN_LLM_VERBOSE_LOGS", "")
	if llmstats.VerboseEnabled(nil) {
		t.Error("expected false with nil flag and no env var")
	}
}

func TestVerboseEnabled_EnvVar(t *testing.T) {
	t.Setenv("MUNINN_LLM_VERBOSE_LOGS", "true")
	if !llmstats.VerboseEnabled(nil) {
		t.Error("expected true when env var set")
	}
}

func TestVerboseEnabled_ConfigFlagTrue(t *testing.T) {
	t.Setenv("MUNINN_LLM_VERBOSE_LOGS", "")
	enabled := true
	if !llmstats.VerboseEnabled(&enabled) {
		t.Error("expected true when config flag is true")
	}
}

func TestVerboseEnabled_ConfigFlagFalse(t *testing.T) {
	t.Setenv("MUNINN_LLM_VERBOSE_LOGS", "")
	disabled := false
	if llmstats.VerboseEnabled(&disabled) {
		t.Error("expected false when config flag is false")
	}
}

func TestVerboseEnabledBool_EnvVar(t *testing.T) {
	t.Setenv("MUNINN_LLM_VERBOSE_LOGS", "true")
	if !llmstats.VerboseEnabledBool(false) {
		t.Error("expected true when env var set, even if bool is false")
	}
}

func TestVerboseEnabledBool_FlagOnly(t *testing.T) {
	t.Setenv("MUNINN_LLM_VERBOSE_LOGS", "")
	if !llmstats.VerboseEnabledBool(true) {
		t.Error("expected true when bool is true")
	}
	if llmstats.VerboseEnabledBool(false) {
		t.Error("expected false when bool is false and no env var")
	}
}
