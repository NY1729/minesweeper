package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func open(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "game.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestChunksSurviveARestart(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	rec := ChunkRecord{ChunkX: -3, ChunkY: 7, Revealed: "ab", Flags: "cd", Owners: `{"5":"alice"}`, Version: 4}
	if err := s.SaveChunks(ctx, "main", []ChunkRecord{rec}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	again, err := OpenSQLite(path) // a new process opening the same file
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if err := again.Init(ctx); err != nil { // Init must be safe to repeat
		t.Fatal(err)
	}
	got, ok, err := again.LoadChunk(ctx, "main", -3, 7)
	if err != nil || !ok || got != rec {
		t.Fatalf("got %+v ok=%v err=%v, want %+v", got, ok, err, rec)
	}
	if _, ok, _ := again.LoadChunk(ctx, "main", 0, 0); ok {
		t.Fatal("an unknown chunk must not exist")
	}
	if _, ok, _ := again.LoadChunk(ctx, "other-world", -3, 7); ok {
		t.Fatal("worlds are separate")
	}
}

func TestAnOlderChunkVersionNeverOverwritesANewerOne(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	s.SaveChunks(ctx, "w", []ChunkRecord{{ChunkX: 1, Revealed: "new", Flags: "f", Owners: "{}", Version: 9}})
	s.SaveChunks(ctx, "w", []ChunkRecord{{ChunkX: 1, Revealed: "old", Flags: "f", Owners: "{}", Version: 3}})
	got, _, _ := s.LoadChunk(ctx, "w", 1, 0)
	if got.Revealed != "new" || got.Version != 9 {
		t.Fatalf("stale write won: %+v", got)
	}
}

func TestUsersScoresAndRanking(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	if err := s.SaveUser(ctx, "a", "px-a", "Alice"); err != nil {
		t.Fatal(err)
	}
	s.SaveUser(ctx, "b", "px-b", "Bob")
	s.AddScores(ctx, map[string]int64{"a": 5, "b": 12, "c": 1, "d": -4}) // c and d have no profile yet
	s.AddScores(ctx, map[string]int64{"a": 10})
	s.SaveUser(ctx, "a", "px-a2", "Alice2") // saving a profile must not reset the score

	got, err := s.LoadUsers(ctx, []string{"a", "b", "nobody"})
	if err != nil || len(got) != 2 || got["a"].Score != 15 || got["a"].Name != "Alice2" || got["a"].Pixels != "px-a2" || got["b"].Score != 12 {
		t.Fatalf("users = %+v err=%v", got, err)
	}
	top, _ := s.TopUsers(ctx, 10)
	if len(top) != 3 || top[0].ID != "a" || top[1].ID != "b" || top[2].ID != "c" {
		t.Fatalf("ranking = %+v (players with score <= 0 must not appear)", top)
	}
	if top, _ := s.TopUsers(ctx, 1); len(top) != 1 {
		t.Fatalf("limit ignored: %+v", top)
	}
}

func TestImportUsersReplacesScoresInsteadOfAddingThem(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	s.ImportUsers(ctx, []User{{ID: "a", Pixels: "p", Name: "A", Score: 100}})
	s.ImportUsers(ctx, []User{{ID: "a", Pixels: "p", Name: "A", Score: 100}}) // running the import twice
	got, _ := s.LoadUsers(ctx, []string{"a"})
	if got["a"].Score != 100 {
		t.Fatalf("score = %d after importing twice, want 100", got["a"].Score)
	}
}

func TestConcurrentUseDoesNotLock(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				c := ChunkRecord{ChunkX: int64(g), ChunkY: int64(i), Revealed: "r", Flags: "f", Owners: "{}", Version: 1}
				if err := s.SaveChunks(ctx, "w", []ChunkRecord{c}); err != nil {
					t.Error(err)
					return
				}
				if _, _, err := s.LoadChunk(ctx, "w", int64(g), int64(i)); err != nil {
					t.Error(err)
					return
				}
				if err := s.AddScores(ctx, map[string]int64{fmt.Sprint("u", g): 1}); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	got, _ := s.LoadUsers(ctx, []string{"u0", "u7"})
	if got["u0"].Score != 50 || got["u7"].Score != 50 {
		t.Fatalf("lost updates: %+v", got)
	}
}
