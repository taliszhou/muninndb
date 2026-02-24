package storage

import (
	"bytes"
	"context"
	"testing"
)

func TestExportImportRoundtrip(t *testing.T) {
	db := openTestPebble(t)
	src := NewPebbleStore(db, 100)

	db2 := openTestPebble(t)
	dst := NewPebbleStore(db2, 100)

	ctx := context.Background()

	ws := src.VaultPrefix("vault-a")
	if err := src.WriteVaultName(ws, "vault-a"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write a few engrams.
	for i := 0; i < 3; i++ {
		eng := &Engram{
			Concept: "concept",
			Content: "content body",
			Tags:    []string{"tag1"},
		}
		if _, err := src.WriteEngram(ctx, ws, eng); err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
	}

	opts := ExportOpts{EmbedderModel: "all-MiniLM-L6-v2", Dimension: 384}

	var buf bytes.Buffer
	result, err := src.ExportVaultData(ctx, ws, "vault-a", opts, &buf)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}
	if result.EngramCount != 3 {
		t.Errorf("EngramCount: got %d, want 3", result.EngramCount)
	}
	if result.TotalKeys == 0 {
		t.Errorf("TotalKeys: expected > 0")
	}

	// Import into a new vault on the destination store.
	wsB := dst.VaultPrefix("vault-b")
	if err := dst.WriteVaultName(wsB, "vault-b"); err != nil {
		t.Fatalf("dst WriteVaultName: %v", err)
	}
	iOpts := ImportOpts{}
	iResult, err := dst.ImportVaultData(ctx, wsB, "vault-b", iOpts, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportVaultData: %v", err)
	}
	if iResult.EngramCount != 3 {
		t.Errorf("ImportVaultData EngramCount: got %d, want 3", iResult.EngramCount)
	}
}

func TestExportEmptyVault(t *testing.T) {
	db := openTestPebble(t)
	src := NewPebbleStore(db, 100)
	ctx := context.Background()

	ws := src.VaultPrefix("empty-vault")
	if err := src.WriteVaultName(ws, "empty-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	opts := ExportOpts{}
	var buf bytes.Buffer
	result, err := src.ExportVaultData(ctx, ws, "empty-vault", opts, &buf)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}
	if result.EngramCount != 0 {
		t.Errorf("expected 0 engrams, got %d", result.EngramCount)
	}

	// Should still be importable.
	db2 := openTestPebble(t)
	dst := NewPebbleStore(db2, 100)
	wsD := dst.VaultPrefix("dest-empty")
	if err := dst.WriteVaultName(wsD, "dest-empty"); err != nil {
		t.Fatalf("dst WriteVaultName: %v", err)
	}
	iResult, err := dst.ImportVaultData(ctx, wsD, "dest-empty", ImportOpts{}, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportVaultData empty: %v", err)
	}
	if iResult.EngramCount != 0 {
		t.Errorf("imported engram count: got %d, want 0", iResult.EngramCount)
	}
}
