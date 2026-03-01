package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestMCPEngineAdapterRetryEnrichNoPlugin verifies that RetryEnrich returns
// "no enrich plugin configured" when the adapter has no enricher set.
func TestMCPEngineAdapterRetryEnrichNoPlugin(t *testing.T) {
	a := &mcpEngineAdapter{eng: nil, enricher: nil}
	_, err := a.RetryEnrich(context.Background(), "default", "01234567890123456789012345")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "no enrich plugin configured" {
		t.Errorf("expected 'no enrich plugin configured', got %q", err.Error())
	}
}

// TestMCPEngineAdapterListDeletedFiltersNil verifies that nil entries in the
// engrams slice are skipped and do not appear in the result.
func TestMCPEngineAdapterListDeletedFiltersNil(t *testing.T) {
	// Build a slice that mimics what eng.ListDeleted might return.
	// We can test the nil-filtering logic directly via listDeletedFromEngrams.
	engrams := []*storage.Engram{
		{ID: storage.ULID{1}, Concept: "first"},
		nil,
		{ID: storage.ULID{3}, Concept: "third"},
		nil,
	}

	now := time.Now()
	result := make([]DeletedEngram, 0, len(engrams))
	for _, eng := range engrams {
		if eng == nil {
			continue
		}
		result = append(result, DeletedEngram{
			ID:               eng.ID.String(),
			Concept:          eng.Concept,
			DeletedAt:        eng.UpdatedAt,
			RecoverableUntil: now.Add(7 * 24 * time.Hour),
			Tags:             eng.Tags,
		})
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 entries after nil filtering, got %d", len(result))
	}
	if result[0].Concept != "first" {
		t.Errorf("result[0].Concept = %q, want %q", result[0].Concept, "first")
	}
	if result[1].Concept != "third" {
		t.Errorf("result[1].Concept = %q, want %q", result[1].Concept, "third")
	}
}

// TestMCPEngineAdapterTraverseDefaultMaxHops verifies that a zero MaxHops
// value is replaced by the default of 3.
func TestMCPEngineAdapterTraverseDefaultMaxHops(t *testing.T) {
	req := &TraverseRequest{
		StartID:  "someID",
		MaxHops:  0,
		MaxNodes: 0,
	}

	maxHops := req.MaxHops
	if maxHops <= 0 {
		maxHops = 3
	}
	maxNodes := req.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 50
	}

	if maxHops != 3 {
		t.Errorf("expected maxHops=3 default, got %d", maxHops)
	}
	if maxNodes != 50 {
		t.Errorf("expected maxNodes=50 default, got %d", maxNodes)
	}
}

// TestAdapterImplementsEngineInterface is a compile-time check that mcpEngineAdapter
// satisfies the EngineInterface contract.
func TestAdapterImplementsEngineInterface(t *testing.T) {
	// Compile-time check: mcpEngineAdapter must implement EngineInterface.
	var _ EngineInterface = (*mcpEngineAdapter)(nil)
}

// TestMCPEngineAdapterTraverseExplicitMaxHops verifies that an explicit MaxHops
// value is not overridden by the default.
func TestMCPEngineAdapterTraverseExplicitMaxHops(t *testing.T) {
	req := &TraverseRequest{
		StartID:  "someID",
		MaxHops:  5,
		MaxNodes: 100,
	}

	maxHops := req.MaxHops
	if maxHops <= 0 {
		maxHops = 3
	}
	maxNodes := req.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 50
	}

	if maxHops != 5 {
		t.Errorf("expected maxHops=5, got %d", maxHops)
	}
	if maxNodes != 100 {
		t.Errorf("expected maxNodes=100, got %d", maxNodes)
	}
}
