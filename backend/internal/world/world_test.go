package world

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

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

// slowChunkStore is a store whose chunk reads take time (like Turso over the network);
// block makes reads of one chunk column wait until its channel is closed.
type slowChunkStore struct {
	*store.MemoryStore
	delay time.Duration
	mu    sync.Mutex
	loads map[int64]int
	block map[int64]chan struct{}
}

func newSlowChunkStore(delay time.Duration) *slowChunkStore {
	return &slowChunkStore{MemoryStore: &store.MemoryStore{}, delay: delay, loads: map[int64]int{}, block: map[int64]chan struct{}{}}
}

func (s *slowChunkStore) LoadChunk(ctx context.Context, w string, x, y int64) (store.ChunkRecord, bool, error) {
	s.mu.Lock()
	s.loads[x]++
	gate := s.block[x]
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	time.Sleep(s.delay)
	return s.MemoryStore.LoadChunk(ctx, w, x, y)
}

func (s *slowChunkStore) loadCount(x int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads[x]
}

func TestConcurrentRequestsShareOneStoreRead(t *testing.T) {
	st := newSlowChunkStore(100 * time.Millisecond)
	m := NewManager("t", "seed", 0, st)
	var wg sync.WaitGroup
	got := make([]*chunk, 20)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = m.getChunk(context.Background(), 3, 4)
		}()
	}
	wg.Wait()
	if n := st.loadCount(3); n != 1 {
		t.Fatalf("20 concurrent requests read the chunk %d times, want 1", n)
	}
	for _, c := range got {
		if c == nil || c != got[0] {
			t.Fatal("everyone must get the same chunk")
		}
	}
}

func TestLoadSurvivesTheCallerGivingUp(t *testing.T) {
	st := newSlowChunkStore(50 * time.Millisecond)
	m := NewManager("t", "seed", 0, st)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var first *chunk
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		first, _ = m.getChunk(ctx, 0, 0)
	}()
	<-started
	time.Sleep(10 * time.Millisecond)
	cancel() // the first requester's connection goes away mid-read
	<-done
	if first == nil {
		t.Fatal("the shared read must not be cancelled with one caller")
	}
}

// A flag on a chunk that is already loaded must not wait for another chunk's slow read:
// the flag path used to hold the global lock while reading from the store.
func TestFlagDoesNotWaitForAnotherChunksLoad(t *testing.T) {
	st := newSlowChunkStore(0)
	m := NewManager("t", "seed", 0, st)
	ctx := context.Background()
	if _, err := m.getChunk(ctx, 1, 0); err != nil { // chunk B is loaded
		t.Fatal(err)
	}
	gate := make(chan struct{})
	st.mu.Lock()
	st.block[0] = gate // chunk A's read hangs until released
	st.mu.Unlock()

	aDone := make(chan struct{})
	go func() {
		m.ToggleFlag(ctx, 5, 5, "a") // chunk (0,0): stuck loading
		close(aDone)
	}()
	time.Sleep(50 * time.Millisecond)

	bDone := make(chan struct{})
	go func() {
		m.ToggleFlag(ctx, ChunkSize+5, 5, "b") // chunk (1,0): already loaded
		close(bDone)
	}()
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatal("a flag on a loaded chunk was blocked by another chunk's store read")
	}
	close(gate)
	select {
	case <-aDone:
	case <-time.After(time.Second):
		t.Fatal("the blocked flag never finished after its chunk loaded")
	}
}

func TestPrefetchOverlapsStoreReads(t *testing.T) {
	st := newSlowChunkStore(100 * time.Millisecond)
	m := NewManager("t", "seed", 0, st)
	var coords [][2]int64
	for x := int64(0); x < 8; x++ {
		coords = append(coords, [2]int64{x, 0})
	}
	start := time.Now()
	m.Prefetch(context.Background(), coords, 8)
	if el := time.Since(start); el > 400*time.Millisecond {
		t.Fatalf("8 reads of 100ms took %v; they should overlap (sequential would be 800ms)", el)
	}
	for x := int64(0); x < 8; x++ {
		if st.loadCount(x) != 1 {
			t.Fatalf("chunk %d read %d times", x, st.loadCount(x))
		}
	}
}

// After an eviction the next flag reloads the chunk and the change is kept and saved.
func TestFlagAfterEvictionIsNotLost(t *testing.T) {
	m := NewManager("t", "seed", 0, &store.MemoryStore{})
	ctx := context.Background()
	m.ToggleFlag(ctx, 5, 5, "a")
	m.DirtySnapshots(10) // saved
	if n := m.Evict(func(int64, int64) bool { return false }); n != 1 {
		t.Fatalf("evicted %d", n)
	}
	if r, err := m.ToggleFlag(ctx, 6, 5, "a"); err != nil || !r.Changed {
		t.Fatalf("flag after eviction failed: %+v %v", r, err)
	}
	if recs := m.DirtySnapshots(10); len(recs) != 1 {
		t.Fatalf("the change must be waiting to be saved, got %d dirty chunks", len(recs))
	}
}
