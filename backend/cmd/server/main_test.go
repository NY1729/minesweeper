package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"minesweeper/backend/internal/store"
	"minesweeper/backend/internal/world"
)

func TestBucket(t *testing.T) {
	b := newBucket(10, 20)
	now := time.Now()
	n := 0
	for i := 0; i < 50; i++ {
		if b.allow(now) {
			n++
		}
	}
	if n != 20 {
		t.Fatalf("burst allowed %d, want 20", n)
	}
	if !b.allow(now.Add(100 * time.Millisecond)) {
		t.Fatal("should refill 1 token after 100ms at 10/s")
	}
	if b.allow(now.Add(100 * time.Millisecond)) {
		t.Fatal("only one token should have refilled")
	}
}

func TestRateLimitsSurviveReconnect(t *testing.T) {
	var r rateLimits
	n := 0
	for i := 0; i < 100; i++ { // 100 "connections", same user
		if r.allow("act:alice", 6, 12) {
			n++
		}
	}
	if n != 12 {
		t.Fatalf("allowed %d across connections, want burst 12", n)
	}
	if !r.allow("act:bob", 6, 12) {
		t.Fatal("other users are unaffected")
	}
}

func TestPixelsRe(t *testing.T) {
	ok := func(s string) bool { return pixelsRe.MatchString(s) }
	rgba := func(px string) string { return strings.Repeat(px, 256) }

	// current format: alpha 0 or f, then r g b
	for _, px := range []string{"0000", "f000", "ffff", "fd45", "f0a9"} {
		if !ok(rgba(px)) {
			t.Fatalf("pixel %q must be accepted", px)
		}
	}
	// legacy format: palette indices 0-31
	for _, c := range []string{"0", "a", "v"} {
		if !ok(strings.Repeat(c, 256)) {
			t.Fatalf("legacy digit %q must be accepted", c)
		}
	}
	bad := map[string]string{
		"semi-transparent": rgba("8123"),
		"uppercase":        rgba("fABC"),
		"non-hex channel":  rgba("f12g"),
		"legacy too high":  strings.Repeat("w", 256),
		"legacy short":     strings.Repeat("a", 255),
		"legacy long":      strings.Repeat("a", 257),
		"rgba short":       rgba("f123")[:1020],
		"rgba long":        rgba("f123") + "f123",
		"mixed lengths":    strings.Repeat("a", 512),
		"empty":            "",
	}
	for name, s := range bad {
		if ok(s) {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

// recordingStore remembers every SaveChunks call; fail makes them error.
type recordingStore struct {
	*store.MemoryStore
	mu    sync.Mutex
	calls [][]store.ChunkRecord
	fail  bool
}

func (r *recordingStore) SaveChunks(_ context.Context, _ string, c []store.ChunkRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("turso down")
	}
	r.calls = append(r.calls, c)
	return nil
}

func dirtyServer(t *testing.T, chunks int) (*server, *recordingStore) {
	t.Helper()
	m := world.NewManager("t", "seed", 0, &store.MemoryStore{})
	for i := 0; i < chunks; i++ {
		if _, err := m.ToggleFlag(context.Background(), int64(i)*world.ChunkSize, 0, "alice"); err != nil { // one chunk each
			t.Fatal(err)
		}
	}
	rs := &recordingStore{MemoryStore: &store.MemoryStore{}}
	return &server{world: m, store: rs, pendingScores: map[string]int64{}}, rs
}

// A backlog must be saved in small batches, never as one giant request: that overloaded
// Turso (every upsert committing separately) and made the retries of the whole backlog pile up.
func TestFlushSavesABacklogInBoundedBatches(t *testing.T) {
	s, rs := dirtyServer(t, 230)
	s.flushAll(context.Background())

	seen := map[int64]bool{}
	for _, call := range rs.calls {
		if len(call) > maxFlushChunks {
			t.Fatalf("a save carried %d chunks, limit is %d", len(call), maxFlushChunks)
		}
		for _, c := range call {
			seen[c.ChunkX] = true
		}
	}
	if len(seen) != 230 || s.world.HasDirty() {
		t.Fatalf("saved %d of 230 chunks, still dirty: %v", len(seen), s.world.HasDirty())
	}
	if len(rs.calls) != 5 {
		t.Fatalf("230 chunks in batches of %d should take 5 saves, took %d", maxFlushChunks, len(rs.calls))
	}
}

func TestFailedFlushRetriesOnlyItsBatchAndLosesNothing(t *testing.T) {
	s, rs := dirtyServer(t, 120)
	rs.fail = true
	if s.flushOnce(context.Background()) {
		t.Fatal("a failed save must report failure")
	}
	s.flushAll(context.Background()) // stops at the first failure instead of hammering the store
	if !s.world.HasDirty() {
		t.Fatal("nothing may be marked saved when saving failed")
	}

	rs.fail = false
	s.flushAll(context.Background())
	seen := map[int64]bool{}
	for _, call := range rs.calls {
		if len(call) > maxFlushChunks {
			t.Fatalf("batch of %d", len(call))
		}
		for _, c := range call {
			seen[c.ChunkX] = true
		}
	}
	if len(seen) != 120 || s.world.HasDirty() {
		t.Fatalf("after recovery saved %d of 120, dirty=%v", len(seen), s.world.HasDirty())
	}
}
