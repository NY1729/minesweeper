package world

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"minesweeper/backend/internal/store"
)

// How long does a client wait for its first screen (7x4 chunks) right after a server start,
// when that area is heavily opened? Logs the split between reading, building and encoding,
// and checks that the remembered mine counts equal the real ones, also after a reload.
func TestColdFirstScreen(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Init(ctx)

	// open a big area at low mine density, so most of the 7x4 chunks end up mostly revealed
	m := NewManager("w", "seed", 20, db)
	for y := int64(0); y < 4*ChunkSize; y += 9 {
		for x := int64(0); x < 7*ChunkSize; x += 9 {
			m.Reveal(ctx, x, y)
		}
	}
	for {
		recs := m.DirtySnapshots(1000)
		if len(recs) == 0 {
			break
		}
		if err := db.SaveChunks(ctx, "w", recs); err != nil {
			t.Fatal(err)
		}
	}

	var coords [][2]int64
	for cy := int64(0); cy < 4; cy++ {
		for cx := int64(0); cx < 7; cx++ {
			coords = append(coords, [2]int64{cx, cy})
		}
	}

	cold := NewManager("w", "seed", 20, db) // a fresh server: nothing in memory
	t0 := time.Now()
	cold.Prefetch(ctx, coords, 8)
	tRead := time.Since(t0)

	t0 = time.Now()
	var snaps []ChunkSnapshot
	cells := 0
	for _, c := range coords {
		s, _ := cold.Snapshot(ctx, c[0], c[1])
		snaps = append(snaps, s)
		cells += len(s.Cells)
		for _, cell := range s.Cells {
			if want := cold.ValueAt(cell.X, cell.Y); cell.Value != want {
				t.Fatalf("cell (%d,%d): snapshot says %d, the real value is %d", cell.X, cell.Y, cell.Value, want)
			}
		}
	}
	for _, c := range coords { // and a snapshot of the original manager, built from values stored while opening
		s, _ := m.Snapshot(ctx, c[0], c[1])
		for _, cell := range s.Cells {
			if want := m.ValueAt(cell.X, cell.Y); cell.Value != want {
				t.Fatalf("after opening: cell (%d,%d): snapshot says %d, real %d", cell.X, cell.Y, cell.Value, want)
			}
		}
	}
	tSnap := time.Since(t0)

	t0 = time.Now()
	bytes := 0
	for _, s := range snaps {
		b, _ := json.Marshal(map[string]any{"type": "chunk", "data": s})
		bytes += len(b)
	}
	tJSON := time.Since(t0)

	t0 = time.Now()
	for _, c := range coords {
		cold.Snapshot(ctx, c[0], c[1]) // a second visitor: chunks are in memory now
	}
	tWarm := time.Since(t0)

	t.Logf("opened cells in the screen: %d of %d", cells, 28*CellsPerChunk)
	t.Logf("read 28 chunks from the database: %v", tRead)
	t.Logf("build 28 snapshots: %v", tSnap)
	t.Logf("encode as JSON: %v  (%d KB sent to each client)", tJSON, bytes/1024)
	t.Logf("the same 28 snapshots for the next visitor: %v", tWarm)
}
