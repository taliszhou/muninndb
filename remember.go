package muninn

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Remember stores a new memory in the given vault and returns its ID.
// concept is a short label (e.g. "Go tips"); content is the full text.
func (db *DB) Remember(ctx context.Context, vault, concept, content string) (string, error) {
	resp, err := db.eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: concept,
		Content: content,
	})
	if err != nil {
		return "", fmt.Errorf("muninndb remember: %w", err)
	}
	return resp.ID, nil
}

// RememberWithType stores a new memory with an explicit MemoryType classification
// and returns its ID.
func (db *DB) RememberWithType(ctx context.Context, vault, concept, content string, memType MemoryType) (string, error) {
	resp, err := db.eng.Write(ctx, &mbp.WriteRequest{
		Vault:      vault,
		Concept:    concept,
		Content:    content,
		MemoryType: uint8(memType),
	})
	if err != nil {
		return "", fmt.Errorf("muninndb remember: %w", err)
	}
	return resp.ID, nil
}

// RememberEnriched stores a new memory with pre-computed enrichment data.
// MuninnDB's background RetroactiveProcessor will skip enrichment stages
// for which data is already provided (Summary, Entities, etc.).
func (db *DB) RememberEnriched(ctx context.Context, vault string, mem EnrichedMemory) (string, error) {
	concept := mem.Concept
	if concept == "" {
		concept = "general"
	}

	req := &mbp.WriteRequest{
		Vault:      vault,
		Concept:    concept,
		Content:    mem.Content,
		MemoryType: uint8(mem.MemoryType),
		TypeLabel:  mem.TypeLabel,
		Summary:    mem.Summary,
		Tags:       mem.Tags,
		Confidence: mem.Confidence,
	}

	for _, e := range mem.Entities {
		req.Entities = append(req.Entities, mbp.InlineEntity{
			Name: e.Name,
			Type: e.Type,
		})
	}
	for _, r := range mem.EntityRelationships {
		req.EntityRelationships = append(req.EntityRelationships, mbp.InlineEntityRelationship{
			FromEntity: r.FromEntity,
			ToEntity:   r.ToEntity,
			RelType:    r.RelType,
			Weight:     r.Weight,
		})
	}

	resp, err := db.eng.Write(ctx, req)
	if err != nil {
		return "", fmt.Errorf("muninndb remember: %w", err)
	}
	return resp.ID, nil
}

// Forget permanently deletes the engram with the given ID from vault.
// Returns [ErrNotFound] if no such engram exists.
func (db *DB) Forget(ctx context.Context, vault, id string) error {
	_, err := db.eng.Forget(ctx, &mbp.ForgetRequest{
		Vault: vault,
		ID:    id,
		Hard:  true,
	})
	if err != nil {
		if isNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("muninndb forget: %w", err)
	}
	return nil
}
