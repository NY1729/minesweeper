package realtime

import (
	"encoding/json"
	"sync"
	"testing"
)

type fakeClient struct {
	mu  sync.Mutex
	got [][]byte
}

func (f *fakeClient) SendJSON(v interface{}) error {
	b, _ := json.Marshal(v)
	return f.SendBytes(b)
}
func (f *fakeClient) SendBytes(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, append([]byte(nil), b...))
	return nil
}

type batch struct {
	Type string   `json:"type"`
	Set  []Cursor `json:"set"`
	Hide []uint64 `json:"hide"`
}

// take returns the cursor batches received since the last call.
func (f *fakeClient) take(t *testing.T) []batch {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []batch
	for _, b := range f.got {
		var m batch
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("invalid json %q: %v", b, err)
		}
		if m.Type == "cursorbatch" {
			out = append(out, m)
		}
	}
	f.got = nil
	return out
}

func newPeer(h *Hub, chunks ...string) (*Peer, *fakeClient) {
	c := &fakeClient{}
	p := NewPeer(c)
	p.SetSubscriptions(chunks)
	h.Add(p)
	return p, c
}

func TestCursorChangesAreBatchedPerViewer(t *testing.T) {
	h := NewHub()
	c1, c2 := ChunkKey(0, 0), ChunkKey(5, 5)
	a, ca := newPeer(h, c1)
	_, cb := newPeer(h, c1)
	_, cc := newPeer(h, c2)

	for i := 0; i < 100; i++ { // one player moving a lot inside a single tick
		h.SetCursor(a, "alice", float64(i), 2, c1, true)
	}
	h.flushCursors()

	got := cb.take(t)
	if len(got) != 1 || len(got[0].Set) != 1 || got[0].Set[0].X != 99 || got[0].Set[0].ID != "alice" {
		t.Fatalf("viewer should get one message with the latest position, got %+v", got)
	}
	if n := len(cc.take(t)); n != 0 {
		t.Fatalf("a viewer of another chunk must get nothing, got %d messages", n)
	}
	if n := len(ca.take(t)); n != 0 {
		t.Fatalf("the mover must not get its own cursor back, got %d messages", n)
	}
	h.flushCursors()
	if n := len(cb.take(t)); n != 0 {
		t.Fatalf("nothing changed, nothing to send; got %d messages", n)
	}
}

func TestCursorLeavingAChunkIsHiddenForItsViewersOnly(t *testing.T) {
	h := NewHub()
	c1, c2 := ChunkKey(0, 0), ChunkKey(1, 0)
	a, _ := newPeer(h, c1)
	_, onlyOld := newPeer(h, c1)
	_, onlyNew := newPeer(h, c2)
	_, both := newPeer(h, c1, c2)

	h.SetCursor(a, "alice", 1, 1, c1, true)
	h.flushCursors()
	onlyOld.take(t)
	both.take(t)

	h.SetCursor(a, "alice", 40, 1, c2, true) // c1 -> c2
	h.flushCursors()
	if g := onlyOld.take(t); len(g) != 1 || len(g[0].Hide) != 1 || len(g[0].Set) != 0 {
		t.Fatalf("viewer of the old chunk must hide it: %+v", g)
	}
	if g := onlyNew.take(t); len(g) != 1 || len(g[0].Set) != 1 || len(g[0].Hide) != 0 {
		t.Fatalf("viewer of the new chunk must show it: %+v", g)
	}
	if g := both.take(t); len(g) != 1 || len(g[0].Set) != 1 || len(g[0].Hide) != 0 {
		t.Fatalf("viewer of both must just see it move, not hide then show: %+v", g)
	}
}

func TestCursorThereAndBackInOneTickIsJustAMove(t *testing.T) {
	h := NewHub()
	c1, c2 := ChunkKey(0, 0), ChunkKey(1, 0)
	a, _ := newPeer(h, c1)
	_, v := newPeer(h, c1)
	h.SetCursor(a, "alice", 1, 1, c1, true)
	h.flushCursors()
	v.take(t)

	h.SetCursor(a, "alice", 40, 1, c2, true)
	h.SetCursor(a, "alice", 2, 1, c1, true)
	h.flushCursors()
	if g := v.take(t); len(g) != 1 || len(g[0].Hide) != 0 || len(g[0].Set) != 1 || g[0].Set[0].X != 2 {
		t.Fatalf("expected one plain update, got %+v", g)
	}
}

func TestHideAndDisconnect(t *testing.T) {
	h := NewHub()
	c1 := ChunkKey(0, 0)
	a, _ := newPeer(h, c1)
	_, v := newPeer(h, c1)

	h.SetCursor(a, "alice", 1, 1, c1, true)
	h.flushCursors()
	v.take(t)
	h.SetCursor(a, "", 0, 0, "", false) // mouse left the board
	h.flushCursors()
	if g := v.take(t); len(g) != 1 || len(g[0].Hide) != 1 {
		t.Fatalf("hide expected: %+v", g)
	}

	h.SetCursor(a, "alice", 3, 3, c1, true)
	h.flushCursors()
	v.take(t)
	h.Remove(a) // disconnect
	h.flushCursors()
	if g := v.take(t); len(g) != 1 || len(g[0].Hide) != 1 {
		t.Fatalf("a disconnect must hide the cursor: %+v", g)
	}
	h.Remove(a)
	h.flushCursors()
	if g := v.take(t); len(g) != 0 {
		t.Fatalf("removing twice must not announce anything: %+v", g)
	}
}

func TestBatchSizeIsCapped(t *testing.T) {
	h := NewHub()
	c1 := ChunkKey(0, 0)
	_, v := newPeer(h, c1)
	for i := 0; i < MaxCursorBatch+50; i++ {
		p, _ := newPeer(h, c1)
		h.SetCursor(p, "u", float64(i), 0, c1, true)
	}
	h.flushCursors()
	if g := v.take(t); len(g) != 1 || len(g[0].Set) != MaxCursorBatch {
		t.Fatalf("a viewer gets at most %d updates per tick, got %+v", MaxCursorBatch, len(g[0].Set))
	}
}

func TestBroadcastEncodesOnceAndRespectsSubscriptions(t *testing.T) {
	h := NewHub()
	c1 := ChunkKey(0, 0)
	_, in1 := newPeer(h, c1)
	_, in2 := newPeer(h, c1)
	_, out := newPeer(h, ChunkKey(9, 9))
	h.BroadcastChunk(c1, map[string]any{"type": "reveal", "n": 1})
	if len(in1.got) != 1 || len(in2.got) != 1 || len(out.got) != 0 {
		t.Fatalf("chunk broadcast reached %d/%d/%d", len(in1.got), len(in2.got), len(out.got))
	}
	if string(in1.got[0]) != string(in2.got[0]) {
		t.Fatal("subscribers must get identical bytes")
	}
	h.BroadcastAll(map[string]any{"type": "ranking"})
	if len(in1.got) != 2 || len(out.got) != 1 {
		t.Fatalf("broadcast-all missed someone: %d %d", len(in1.got), len(out.got))
	}
}

func TestSendToUserReachesEveryConnectionOfThatUserOnly(t *testing.T) {
	h := NewHub()
	a1, c1 := newPeer(h)
	a2, c2 := newPeer(h)
	b, cb := newPeer(h)
	h.Bind(a1, "alice")
	h.Bind(a2, "alice") // same person, second tab
	h.Bind(b, "bob")

	h.SendToUser("alice", map[string]any{"type": "score", "me": 7})
	if len(c1.got) != 1 || len(c2.got) != 1 || len(cb.got) != 0 {
		t.Fatalf("alice's tabs got %d/%d, bob got %d", len(c1.got), len(c2.got), len(cb.got))
	}

	h.Remove(a1) // that tab closes
	h.SendToUser("alice", map[string]any{"type": "score", "me": 8})
	if len(c1.got) != 1 || len(c2.got) != 2 {
		t.Fatalf("a closed tab must not be sent to: %d %d", len(c1.got), len(c2.got))
	}
	h.Remove(a2)
	h.SendToUser("alice", map[string]any{"type": "score", "me": 9}) // nobody left: no panic
	if len(h.byUser) != 1 {
		t.Fatalf("empty users must be forgotten, have %d", len(h.byUser))
	}
}
