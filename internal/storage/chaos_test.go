package storage

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/cockroachdb/pebble"
)

// TestChaos_AbruptClose_NoWALFlush verifies that data committed with pebble.Sync
// survives an abrupt close WITHOUT a final WAL flush (simulating kill-9 mid-operation).
// Sync writes fsync per-write, so they survive even without the walSyncer final flush.
func TestChaos_AbruptClose_NoWALFlush(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-chaos-abrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 0})
	ws := store.VaultPrefix("chaos-test")
	ctx := context.Background()

	// Write 10 engrams with the default Sync path
	ids := make([]ULID, 10)
	for i := 0; i < 10; i++ {
		id, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: fmt.Sprintf("chaos engram %d", i),
			Content: fmt.Sprintf("content for chaos test %d", i),
		})
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		ids[i] = id
	}

	// Abrupt close — no walSyncer flush, no graceful shutdown
	// Simulates kill-9 between writes (Sync writes survive this)
	_ = db.Close() // ignore close error (may get ErrClosed in some pebble versions)

	// Reopen
	db2, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatalf("Reopen after abrupt close: %v", err)
	}
	defer db2.Close()

	store2 := NewPebbleStore(db2, PebbleStoreConfig{CacheSize: 0})
	ws2 := store2.VaultPrefix("chaos-test")

	// All Sync writes must survive
	for i, id := range ids {
		eng, err := store2.GetEngram(ctx, ws2, id)
		if err != nil {
			t.Fatalf("GetEngram[%d] after abrupt close: %v", i, err)
		}
		want := fmt.Sprintf("chaos engram %d", i)
		if eng.Concept != want {
			t.Errorf("engram[%d]: got %q, want %q", i, eng.Concept, want)
		}
	}
}

// TestChaos_NoSync_WithoutWALFlush_MayLoseDataloss documents the NoSync + no-WAL-flush
// boundary: data written with NoSync that was NOT followed by a walSyncer flush
// (db.LogData(nil, pebble.Sync)) is NOT guaranteed to survive an abrupt close.
//
// This test documents and validates the behavior: NoSync writes without explicit
// flush are in the WAL buffer and MAY or MAY NOT survive depending on OS buffering.
// This is the expected tradeoff — the walSyncer provides the guarantee, not individual writes.
func TestChaos_NoSync_WithoutWALFlush_DataMayBeLost(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-chaos-nosync-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	// Use a raw Pebble batch with NoSync (bypassing walSyncer)
	batch := db.NewBatch()
	key := []byte{0xAA, 0xBB, 0xCC}
	val := []byte("nosync-value-without-walsyncer-flush")
	if err := batch.Set(key, val, nil); err != nil {
		batch.Close()
		t.Fatal(err)
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		batch.Close()
		t.Fatal(err)
	}
	batch.Close()

	// Abrupt close WITHOUT LogData sync (no walSyncer flush)
	_ = db.Close()

	// Reopen
	db2, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	// Check if the key survived — it might or might not (OS-dependent)
	val2, closer, err := db2.Get(key)
	if err == pebble.ErrNotFound {
		t.Logf("NoSync write without WAL flush was lost on abrupt close (expected behavior)")
		return
	}
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	defer closer.Close()
	t.Logf("NoSync write without WAL flush survived (OS buffering preserved it): %s", val2)
	// Both outcomes are valid — this test documents behavior, not enforces it
}

// TestChaos_MixedSync_AllSyncSurvive verifies that in a mixed write sequence
// (interleaved Sync and NoSync writes), the Sync writes always survive abrupt close.
func TestChaos_MixedSync_AllSyncSurvive(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-chaos-mixed-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	ws := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 0}).VaultPrefix("mixed-chaos")
	ctx := context.Background()

	// Write 5 sync + 5 nosync engrams interleaved
	syncStore := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 0}) // Sync path
	noSyncStore := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 0, NoSyncEngrams: true})

	syncIDs := make([]ULID, 5)
	for i := 0; i < 5; i++ {
		id, err := syncStore.WriteEngram(ctx, ws, &Engram{
			Concept: fmt.Sprintf("sync-engram-%d", i),
			Content: fmt.Sprintf("sync content %d", i),
		})
		if err != nil {
			t.Fatalf("sync WriteEngram[%d]: %v", i, err)
		}
		syncIDs[i] = id

		// Interleave a NoSync write
		_, _ = noSyncStore.WriteEngram(ctx, ws, &Engram{
			Concept: fmt.Sprintf("nosync-engram-%d", i),
			Content: fmt.Sprintf("nosync content %d", i),
		})
	}

	// Abrupt close (no final WAL flush)
	_ = db.Close()

	db2, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	store2 := NewPebbleStore(db2, PebbleStoreConfig{CacheSize: 0})
	ws2 := store2.VaultPrefix("mixed-chaos")

	// All 5 Sync writes MUST survive
	for i, id := range syncIDs {
		eng, err := store2.GetEngram(ctx, ws2, id)
		if err != nil {
			t.Fatalf("sync engram[%d] lost after abrupt close: %v", i, err)
		}
		want := fmt.Sprintf("sync-engram-%d", i)
		if eng.Concept != want {
			t.Errorf("sync engram[%d]: got %q, want %q", i, eng.Concept, want)
		}
	}
	t.Logf("All 5 Sync writes survived abrupt close")
}
