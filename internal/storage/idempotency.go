package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// IdempotencyReceipt is the value stored at an idempotency key.
type IdempotencyReceipt struct {
	EngramID  string `json:"engram_id"`
	CreatedAt int64  `json:"created_at"` // unix nanos
}

// CheckIdempotency looks up an op_id receipt. Returns nil, nil if not found.
func (ps *PebbleStore) CheckIdempotency(ctx context.Context, opID string) (*IdempotencyReceipt, error) {
	key := keys.IdempotencyKey(opID)
	val, err := Get(ps.db, key)
	if err != nil {
		return nil, fmt.Errorf("check idempotency: %w", err)
	}
	if val == nil {
		return nil, nil
	}
	var receipt IdempotencyReceipt
	if err := json.Unmarshal(val, &receipt); err != nil {
		return nil, fmt.Errorf("decode idempotency receipt: %w", err)
	}
	return &receipt, nil
}

// WriteIdempotency writes an idempotency receipt for op_id → engramID.
func (ps *PebbleStore) WriteIdempotency(ctx context.Context, opID, engramID string) error {
	receipt := IdempotencyReceipt{
		EngramID:  engramID,
		CreatedAt: time.Now().UnixNano(),
	}
	val, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("marshal idempotency receipt: %w", err)
	}
	key := keys.IdempotencyKey(opID)
	return ps.db.Set(key, val, pebble.NoSync)
}
