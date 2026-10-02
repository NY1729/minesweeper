package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// MaxCursorsPerPeer caps the cursor list sent after a subscribe.
const MaxCursorsPerPeer = 50

// MaxCursorBatch caps how many cursor updates one viewer gets per tick. Beyond that the
// rest are skipped until those players move again; it bounds the work per viewer in a crowd.
const MaxCursorBatch = 100

type Cursor struct {
	Seq uint64  `json:"s"`  // per connection, so two tabs of one user are two cursors
	ID  string  `json:"id"` // owner's public user id
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
}

type Client interface {
	SendJSON(v interface{}) error
	// SendBytes sends an already encoded JSON text message, so a broadcast encodes once.
	SendBytes(b []byte) error
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*Peer]struct{}

	cmu     sync.Mutex
	pending map[uint64]*cursorChange // cursor changes since the last flush, by connection
}

type Peer struct {
	Client
	mu   sync.RWMutex
	subs map[string]struct{}

	seq         uint64
	cursor      *Cursor
	cursorChunk string
}

// cursorChange is everything that happened to one cursor since the last flush.
type cursorChange struct {
	id       string
	x, y     float64
	visible  bool
	chunk    string // where it is now
	hadOld   bool   // it was visible at the start of the window,
	oldChunk string // in this chunk: that is what viewers currently show
}

var peerSeq atomic.Uint64

func NewHub() *Hub {
	return &Hub{clients: make(map[*Peer]struct{}), pending: make(map[uint64]*cursorChange)}
}
func NewPeer(c Client) *Peer {
	return &Peer{Client: c, subs: make(map[string]struct{}), seq: peerSeq.Add(1)}
}

func ChunkKey(cx, cy int64) string {
	b, _ := json.Marshal([2]int64{cx, cy})
	return string(b)
}

func (p *Peer) SetSubscriptions(keys []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subs = make(map[string]struct{}, len(keys))
	for _, k := range keys {
		p.subs[k] = struct{}{}
	}
}

func (p *Peer) Subscribed(key string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.subs[key]
	return ok
}

func (h *Hub) Add(p *Peer) {
	h.mu.Lock()
	h.clients[p] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) Remove(p *Peer) {
	h.mu.Lock()
	delete(h.clients, p)
	h.mu.Unlock()
	h.SetCursor(p, "", 0, 0, "", false)
}

// SetCursor records p's selected cell (visible=false hides it). chunk is the ChunkKey under
// the cell. Viewers are told by the next CursorLoop tick, which batches many changes into
// one message per viewer instead of one message per change.
func (h *Hub) SetCursor(p *Peer, userID string, x, y float64, chunk string, visible bool) {
	p.mu.Lock()
	had, oldChunk := p.cursor != nil, p.cursorChunk
	if visible {
		p.cursor = &Cursor{Seq: p.seq, ID: userID, X: x, Y: y}
		p.cursorChunk = chunk
	} else {
		p.cursor = nil
	}
	p.mu.Unlock()
	if !visible && !had {
		return // nothing was shown, nothing to hide
	}

	h.cmu.Lock()
	c := h.pending[p.seq]
	if c == nil {
		// first change in this window: remember what viewers see right now
		c = &cursorChange{hadOld: had, oldChunk: oldChunk}
		h.pending[p.seq] = c
	}
	c.id, c.x, c.y, c.visible, c.chunk = userID, x, y, visible, chunk
	h.cmu.Unlock()
}

// CursorLoop sends the batched cursor changes every tick until ctx ends.
func (h *Hub) CursorLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.flushCursors()
		}
	}
}

// flushCursors sends each viewer one {"type":"cursorbatch","set":[...],"hide":[...]} with
// the cursors that changed inside its subscribed chunks. Each cursor is encoded once and
// the pieces are joined per viewer, so the cost per viewer is mostly copying bytes.
func (h *Hub) flushCursors() {
	h.cmu.Lock()
	if len(h.pending) == 0 {
		h.cmu.Unlock()
		return
	}
	changes := h.pending
	h.pending = make(map[uint64]*cursorChange)
	h.cmu.Unlock()

	type entry struct {
		seq  uint64
		json []byte
	}
	sets := make(map[string][]entry)  // by the chunk the cursor is in now
	hides := make(map[string][]entry) // by the chunk viewers last saw it in
	for seq, c := range changes {
		if c.visible {
			b, _ := json.Marshal(Cursor{Seq: seq, ID: c.id, X: c.x, Y: c.y})
			sets[c.chunk] = append(sets[c.chunk], entry{seq, b})
		}
		if c.hadOld && (!c.visible || c.oldChunk != c.chunk) {
			hides[c.oldChunk] = append(hides[c.oldChunk], entry{seq, strconv.AppendUint(nil, seq, 10)})
		}
	}

	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.clients))
	for p := range h.clients {
		peers = append(peers, p)
	}
	h.mu.RUnlock()

	var buf bytes.Buffer
	for _, p := range peers {
		var set, hide [][]byte
		var shown map[uint64]struct{}
		p.mu.RLock()
		for k := range p.subs {
			for _, e := range sets[k] {
				if e.seq == p.seq || len(set) >= MaxCursorBatch {
					continue
				}
				set = append(set, e.json)
				if shown == nil {
					shown = make(map[uint64]struct{})
				}
				shown[e.seq] = struct{}{}
			}
		}
		for k := range p.subs {
			for _, e := range hides[k] {
				// hide only if it left this viewer's sight (not just moved between two watched chunks)
				if _, still := shown[e.seq]; e.seq != p.seq && !still {
					hide = append(hide, e.json)
				}
			}
		}
		p.mu.RUnlock()
		if len(set) == 0 && len(hide) == 0 {
			continue
		}
		buf.Reset()
		buf.WriteString(`{"type":"cursorbatch","set":[`)
		buf.Write(bytes.Join(set, []byte(",")))
		buf.WriteString(`],"hide":[`)
		buf.Write(bytes.Join(hide, []byte(",")))
		buf.WriteString(`]}`)
		_ = p.SendBytes(append([]byte(nil), buf.Bytes()...))
	}
}

// CursorsFor lists other peers' cursors inside p's subscribed chunks,
// sent once after a subscribe; later changes arrive as cursor batches.
func (h *Hub) CursorsFor(p *Peer) []Cursor {
	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.clients))
	for q := range h.clients {
		if q != p {
			peers = append(peers, q)
		}
	}
	h.mu.RUnlock()
	out := make([]Cursor, 0)
	for _, q := range peers {
		q.mu.RLock()
		c, chunk := q.cursor, q.cursorChunk
		q.mu.RUnlock()
		if c != nil && p.Subscribed(chunk) {
			out = append(out, *c)
			if len(out) >= MaxCursorsPerPeer {
				break
			}
		}
	}
	return out
}

// BroadcastChunk sends msg to everyone subscribed to the chunk; the message is encoded once.
func (h *Hub) BroadcastChunk(key string, msg interface{}) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.clients))
	for p := range h.clients {
		if p.Subscribed(key) {
			peers = append(peers, p)
		}
	}
	h.mu.RUnlock()
	for _, p := range peers {
		_ = p.SendBytes(b)
	}
}

// BroadcastAll sends msg to every connected peer; the message is encoded once.
func (h *Hub) BroadcastAll(msg interface{}) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.clients))
	for p := range h.clients {
		peers = append(peers, p)
	}
	h.mu.RUnlock()
	for _, p := range peers {
		_ = p.SendBytes(b)
	}
}

func (h *Hub) SubscribedKeys() map[string]struct{} {
	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.clients))
	for p := range h.clients {
		peers = append(peers, p)
	}
	h.mu.RUnlock()
	out := make(map[string]struct{})
	for _, p := range peers {
		p.mu.RLock()
		for k := range p.subs {
			out[k] = struct{}{}
		}
		p.mu.RUnlock()
	}
	return out
}
