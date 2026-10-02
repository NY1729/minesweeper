package world

import (
	"context"
	"math/rand"
	"testing"

	"minesweeper/backend/internal/store"
)

func TestFlagOwnership(t *testing.T) {
	m := NewManager("t", "seed", 0, &store.MemoryStore{})
	ctx := context.Background()

	if r, _ := m.ToggleFlag(ctx, 5, -3, "alice"); !r.On || !r.Changed {
		t.Fatal("alice should place flag")
	}
	if r, _ := m.ToggleFlag(ctx, 5, -3, "bob"); !r.On || r.Owner != "alice" || r.Changed {
		t.Fatal("bob must not remove alice's flag")
	}
	snap, _ := m.Snapshot(ctx, 0, -1)
	if len(snap.Flags) != 1 || snap.Flags[0].Owner != "alice" {
		t.Fatalf("snapshot flags = %+v", snap.Flags)
	}
	recs := m.DirtySnapshots(100)
	if len(recs) != 1 || recs[0].Owners == "{}" {
		t.Fatalf("owners not persisted: %+v", recs)
	}
	if r, _ := m.ToggleFlag(ctx, 5, -3, "alice"); r.On {
		t.Fatal("alice should remove her flag")
	}
}

func TestEvictKeepsDirtyAndSubscribed(t *testing.T) {
	m := NewManager("t", "seed", 0, &store.MemoryStore{})
	ctx := context.Background()
	m.ToggleFlag(ctx, 0, 0, "a") // chunk 0:0, dirty
	m.Snapshot(ctx, 1, 0)        // chunk 1:0, clean
	m.Snapshot(ctx, 2, 0)        // chunk 2:0, clean, subscribed
	if n := m.Evict(func(cx, cy int64) bool { return cx == 2 }); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	m.DirtySnapshots(100) // saved -> clean
	if n := m.Evict(func(int64, int64) bool { return false }); n != 2 {
		t.Fatalf("evicted %d, want 2", n)
	}
}

// Clicking random cells must lose points on average, or a bot can farm the ranking.
func TestRandomClicksLoseScore(t *testing.T) {
	for _, permille := range []uint64{60, 100, 160, 250, 400} {
		m := NewManager("t", "seed", permille, &store.MemoryStore{})
		rng := rand.New(rand.NewSource(1))
		var total, clicks int64
		for i := 0; i < 1500; i++ {
			x, y := rng.Int63n(1<<30), rng.Int63n(1<<30)
			cells, _ := m.Reveal(context.Background(), x, y)
			if len(cells) == 0 {
				continue
			}
			clicks++
			total += m.ScoreDelta(x, y, cells)
		}
		if avg := float64(total) / float64(clicks); avg >= 0 {
			t.Fatalf("density %d: random click averages %+.2f points", permille, avg)
		}
	}
}

func TestScoreDelta(t *testing.T) {
	m := NewManager("t", "seed", 160, &store.MemoryStore{})
	if got := m.ScoreDelta(3, 4, []Cell{{X: 3, Y: 4, Value: 2}}); got != 1 {
		t.Fatalf("safe cell: %d", got)
	}
	flood := []Cell{{X: 3, Y: 4, Value: 0}, {X: 4, Y: 4, Value: 1}, {X: 2, Y: 4, Value: 1}}
	if got := m.ScoreDelta(3, 4, flood); got != 1 {
		t.Fatalf("flood must still be +1, got %d", got)
	}
	if got := m.ScoreDelta(3, 4, []Cell{{X: 3, Y: 4, Value: MineValue}}); got != -10 {
		t.Fatalf("mine: %d", got)
	}
	if got := m.ScoreDelta(3, 4, nil); got != 0 {
		t.Fatalf("nothing opened: %d", got)
	}
	if low := NewManager("t", "seed", 50, &store.MemoryStore{}).MinePenalty(); low <= 10 {
		t.Fatalf("penalty must grow as mines get rarer, got %d", low)
	}
}

func TestDirtySnapshotsIsBoundedAndKeepsTheRestDirty(t *testing.T) {
	ctx := context.Background()
	m := NewManager("t", "seed", 0, &store.MemoryStore{})
	for i := int64(0); i < 10; i++ {
		m.ToggleFlag(ctx, i*ChunkSize, 0, "a") // ten different chunks
	}
	first := m.DirtySnapshots(4)
	if len(first) != 4 || !m.HasDirty() {
		t.Fatalf("limit 4 returned %d, hasDirty=%v", len(first), m.HasDirty())
	}
	m.MarkDirty(first) // a failed save puts exactly those back
	seen := map[int64]bool{}
	for total := 0; m.HasDirty() && total < 20; total++ {
		for _, r := range m.DirtySnapshots(4) {
			seen[r.ChunkX] = true
		}
	}
	if len(seen) != 10 || m.HasDirty() {
		t.Fatalf("drained %d of 10 chunks, dirty=%v", len(seen), m.HasDirty())
	}
}
