package storage

import (
	"encoding/binary"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

const counterFlushInterval = 100 * time.Millisecond

// counterCoalescer replaces per-write goroutines for vault count persistence.
// Submit is lock-free via sync.Map + atomic.Int64 — never drops updates.
// The flusher sweeps the map every 100ms and writes to Pebble with NoSync.
// The in-memory atomic is authoritative; last-write-wins is correct for a
// monotonic counter. On crash, getOrInitCounter falls back to a full scan.
type counterCoalescer struct {
	db   *pebble.DB
	m    sync.Map // [8]byte → *atomic.Int64
	stop chan struct{}
	done chan struct{}
}

func newCounterCoalescer(db *pebble.DB) *counterCoalescer {
	c := &counterCoalescer{
		db:   db,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go c.run()
	return c
}

// Submit records a counter update. Lock-free and never drops.
func (c *counterCoalescer) Submit(wsPrefix [8]byte, count int64) {
	// Fast path: key already exists — just update the atomic.
	if v, ok := c.m.Load(wsPrefix); ok {
		v.(*atomic.Int64).Store(count)
		return
	}
	// Slow path: first update for this vault.
	ai := new(atomic.Int64)
	ai.Store(count)
	if actual, loaded := c.m.LoadOrStore(wsPrefix, ai); loaded {
		// Lost the race — another goroutine stored first.
		actual.(*atomic.Int64).Store(count)
	}
}

func (c *counterCoalescer) run() {
	defer close(c.done)

	ticker := time.NewTicker(counterFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.flush()
		case <-c.stop:
			c.flush()
			return
		}
	}
}

func (c *counterCoalescer) flush() {
	defer func() {
		if r := recover(); r != nil {
			msg, ok := r.(string)
			if ok && (strings.Contains(msg, "pebble: closed") || strings.Contains(msg, "pebble: cleaned up")) {
				// Known teardown panic — swallow silently.
				return
			}
			slog.Warn("storage: unexpected panic in counter flush", "panic", r)
		}
	}()
	var buf [8]byte
	c.m.Range(func(k, v any) bool {
		ws := k.([8]byte)
		count := v.(*atomic.Int64).Load()
		binary.BigEndian.PutUint64(buf[:], uint64(count))
		if err := c.db.Set(keys.VaultCountKey(ws), buf[:], pebble.NoSync); err != nil {
			slog.Warn("storage: counter flush failed", "err", err)
		}
		c.m.Delete(ws)
		return true
	})
}

// Delete removes any pending counter entry for ws so a stale flush cannot
// write back a count that was already removed (e.g., during ClearVault).
func (c *counterCoalescer) Delete(ws [8]byte) {
	c.m.Delete(ws)
}

// Close performs a final flush and waits for the flusher goroutine to exit.
// Must be called before db.Close().
func (c *counterCoalescer) Close() {
	close(c.stop)
	<-c.done
}
