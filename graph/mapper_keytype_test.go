package graph

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestMapper_KeyTypes_RoundTrip verifies that for string, int64, and
// [16]byte keys, Intern→Lookup→Resolve is an identity round-trip.
func TestMapper_KeyTypes_RoundTrip(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		t.Parallel()
		m := NewMapper[string]()
		keys := []string{"", "a", "hello", "world", "foo", "bar", strings.Repeat("x", 256)}

		// First pass: Intern and record.
		ids := make(map[string]NodeID, len(keys))
		for _, k := range keys {
			ids[k] = m.Intern(k)
		}
		// Second Intern must be stable.
		for _, k := range keys {
			if got := m.Intern(k); got != ids[k] {
				t.Errorf("Intern(%q) drift: %d -> %d", k, ids[k], got)
			}
		}
		// Lookup must equal Intern result.
		for _, k := range keys {
			got, ok := m.Lookup(k)
			if !ok {
				t.Errorf("Lookup(%q): not found", k)
			} else if got != ids[k] {
				t.Errorf("Lookup(%q) = %d, want %d", k, got, ids[k])
			}
		}
		// Resolve must return the original key.
		for _, k := range keys {
			v, ok := m.Resolve(ids[k])
			if !ok || v != k {
				t.Errorf("Resolve(%d) = (%q, %v), want (%q, true)", ids[k], v, ok, k)
			}
		}
		// Unknown key must return ok=false.
		if _, ok := m.Lookup("not-interned"); ok {
			t.Error("Lookup(unknown) returned ok=true")
		}
	})

	t.Run("int64", func(t *testing.T) {
		t.Parallel()
		m := NewMapper[int64]()
		keys := []int64{0, 1, -1, 1 << 62, -(1 << 62), 42, 100, 999}

		ids := make(map[int64]NodeID, len(keys))
		for _, k := range keys {
			ids[k] = m.Intern(k)
		}
		for _, k := range keys {
			if got := m.Intern(k); got != ids[k] {
				t.Errorf("Intern(%d) drift: %d -> %d", k, ids[k], got)
			}
		}
		for _, k := range keys {
			got, ok := m.Lookup(k)
			if !ok || got != ids[k] {
				t.Errorf("Lookup(%d) = (%d, %v), want (%d, true)", k, got, ok, ids[k])
			}
			v, ok := m.Resolve(ids[k])
			if !ok || v != k {
				t.Errorf("Resolve(%d) = (%d, %v), want (%d, true)", ids[k], v, ok, k)
			}
		}
		if _, ok := m.Lookup(int64(99999999)); ok {
			t.Error("Lookup(unknown int64) returned ok=true")
		}
	})

	t.Run("uuid_like", func(t *testing.T) {
		t.Parallel()
		// Use [16]byte directly to exercise the dedicated fast path in
		// mapperShardFor (not the fmt.Fprintf fallback).
		m := NewMapper[[16]byte]()
		var keys [][16]byte
		for i := 0; i < 100; i++ {
			var k [16]byte
			k[0] = byte(i)
			k[15] = byte(i ^ 0xAB)
			keys = append(keys, k)
		}

		ids := make(map[[16]byte]NodeID, len(keys))
		for _, k := range keys {
			ids[k] = m.Intern(k)
		}
		for _, k := range keys {
			got, ok := m.Lookup(k)
			if !ok || got != ids[k] {
				t.Errorf("Lookup(%v) = (%d, %v), want (%d, true)", k, got, ok, ids[k])
			}
			v, ok := m.Resolve(ids[k])
			if !ok || v != k {
				t.Errorf("Resolve(%d) = (%v, %v), want (%v, true)", ids[k], v, ok, k)
			}
		}
		// Unknown key must return ok=false.
		var unknown [16]byte
		unknown[0] = 0xFF
		if _, ok := m.Lookup(unknown); ok {
			t.Error("Lookup(unknown [16]byte) returned ok=true")
		}
	})
}

// TestMapper_KeyTypes_Concurrent runs 16 goroutines that all intern the
// same int64 corpus and asserts that every returned NodeID is consistent
// with a subsequent Lookup — exercising the double-check inside internSlow
// under real scheduler concurrency.
func TestMapper_KeyTypes_Concurrent(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 16
		corpusSize = 512
	)

	m := NewMapper[int64]()

	// Build the corpus first so all goroutines operate on identical keys.
	corpus := make([]int64, corpusSize)
	for i := range corpus {
		corpus[i] = int64(i)
	}

	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		errored atomic.Int64
	)
	start.Add(1)
	done.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer done.Done()
			start.Wait()
			for _, k := range corpus {
				id := m.Intern(k)
				// Lookup must agree with the id returned by Intern.
				got, ok := m.Lookup(k)
				if !ok || got != id {
					errored.Add(1)
					return
				}
				// Repeated Intern must be stable.
				if id2 := m.Intern(k); id2 != id {
					errored.Add(1)
					return
				}
			}
		}()
	}

	start.Done()
	done.Wait()

	if n := errored.Load(); n != 0 {
		t.Fatalf("%d goroutine(s) reported inconsistencies", n)
	}
	if got, want := m.Len(), corpusSize; got != want {
		t.Fatalf("Len = %d, want %d (no duplicates expected)", got, want)
	}
}
