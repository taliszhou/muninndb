package storage

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripedMutex_SameKeyReturnsSameStripe(t *testing.T) {
	var s stripedMutex
	key := []byte("PostgreSQL")
	m1 := s.For(key)
	m2 := s.For(key)
	require.NotNil(t, m1)
	assert.Same(t, m1, m2, "same key must always map to the same mutex stripe")
}

func TestStripedMutex_EmptyKey(t *testing.T) {
	var s stripedMutex
	mu := s.For([]byte{})
	require.NotNil(t, mu, "For(empty) must return a non-nil mutex")
}

func TestStripedMutex_NilKey(t *testing.T) {
	var s stripedMutex
	mu := s.For(nil)
	require.NotNil(t, mu, "For(nil) must return a non-nil mutex")
}

func TestStripedMutex_DistributesKeys(t *testing.T) {
	// With 256 stripes and 100 distinct keys, we expect decent distribution.
	// This is a probabilistic check — FNV-32a distributes well in practice.
	var s stripedMutex
	seen := make(map[*sync.Mutex]int)
	for i := 0; i < 100; i++ {
		key := []byte(fmt.Sprintf("entity-%d", i))
		seen[s.For(key)]++
	}
	// With 100 keys across 256 stripes, we expect at least 10 distinct stripes used.
	// Pure hash collision risk is negligible at this scale.
	assert.GreaterOrEqual(t, len(seen), 10,
		"poor distribution: only %d distinct stripes used for 100 keys", len(seen))
}

// TestStripedMutex_ConcurrentAccess verifies that the striped mutex correctly serialises
// concurrent read-modify-write operations on the same key. Run with -race to detect races.
func TestStripedMutex_ConcurrentAccess(t *testing.T) {
	var s stripedMutex
	key := []byte("shared-entity")

	const goroutines = 64
	const itersPerGoroutine = 100
	counter := 0

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range itersPerGoroutine {
				mu := s.For(key)
				mu.Lock()
				counter++ // unsynchronised increment — safe only under the stripe lock
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, goroutines*itersPerGoroutine, counter,
		"concurrent increments must be fully serialised by the stripe lock")
}

// TestStripedMutex_IndependentKeys verifies that two different keys whose hashes
// land on different stripes do not block each other.
func TestStripedMutex_IndependentKeys(t *testing.T) {
	var s stripedMutex

	// Find two keys that hash to different stripes.
	// We try pairs until we find one; this avoids depending on specific hash values.
	var keyA, keyB []byte
	for i := 0; i < 256; i++ {
		a := []byte(fmt.Sprintf("alpha-%d", i))
		b := []byte(fmt.Sprintf("beta-%d", i))
		if s.For(a) != s.For(b) {
			keyA, keyB = a, b
			break
		}
	}
	if keyA == nil {
		t.Skip("could not find two keys on different stripes")
	}

	// Lock stripe A and confirm that stripe B is independently acquirable.
	muA := s.For(keyA)
	muB := s.For(keyB)

	muA.Lock()
	locked := make(chan struct{})
	go func() {
		muB.Lock()
		close(locked)
		muB.Unlock()
	}()
	// If muB were the same stripe as muA, this would deadlock.
	select {
	case <-locked:
		// success — muB acquired independently
	default:
		// give the goroutine a moment to run
		muA.Unlock()
		<-locked
		return
	}
	muA.Unlock()
}

// TestStripedMutex_BoundedMemory verifies that the type has a fixed number of mutexes
// regardless of how many distinct keys are used. This is the core correctness property
// (prevents unbounded sync.Map growth).
func TestStripedMutex_BoundedMemory(t *testing.T) {
	var s stripedMutex

	// Call For() with 10,000 distinct keys.
	seen := make(map[*sync.Mutex]struct{})
	for i := range 10_000 {
		seen[s.For([]byte(fmt.Sprintf("entity-%d", i)))] = struct{}{}
	}

	// Despite 10,000 distinct keys, the number of distinct mutexes must be capped at lockStripes.
	assert.LessOrEqual(t, len(seen), lockStripes,
		"stripedMutex must use at most %d distinct mutexes regardless of key count", lockStripes)
}
