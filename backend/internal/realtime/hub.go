package realtime

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// MaxCursorsPerPeer caps the cursor list sent after a subscribe.
const MaxCursorsPerPeer = 50

type Cursor struct {
	Seq uint64  `json:"s"`  // per connection, so two tabs of one user are two cursors
	ID  string  `json:"id"` // owner's public user id
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
}

type Client interface {
	SendJSON(v interface{}) error
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*Peer]struct{}
}

type Peer struct {
	Client
	mu   sync.RWMutex
	subs map[string]struct{}

	seq         uint64
	cursor      *Cursor
	cursorChunk string
}

var peerSeq atomic.Uint64

func NewHub() *Hub { return &Hub{clients: make(map[*Peer]struct{})} }
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

// SetCursor records p's selected cell and immediately relays the change to peers
// viewing that chunk. visible=false hides it. chunk is the ChunkKey under the cell.
func (h *Hub) SetCursor(p *Peer, userID string, x, y float64, chunk string, visible bool) {
	p.mu.Lock()
	oldChunk, had := p.cursorChunk, p.cursor != nil
	if visible {
		p.cursor = &Cursor{Seq: p.seq, ID: userID, X: x, Y: y}
		p.cursorChunk = chunk
	} else {
		p.cursor = nil
	}
	cur := p.cursor
	p.mu.Unlock()

	// Moved to another chunk (or hidden): tell the old chunk's viewers to drop it.
	if had && (!visible || oldChunk != chunk) {
		h.broadcastChunkExcept(oldChunk, p, map[string]interface{}{"type": "cursor", "s": p.seq, "hide": true})
	}
	if cur != nil {
		h.broadcastChunkExcept(chunk, p, map[string]interface{}{"type": "cursor", "s": cur.Seq, "id": cur.ID, "x": cur.X, "y": cur.Y})
	}
}

// CursorsFor lists other peers' cursors inside p's subscribed chunks,
// sent once after a subscribe; later changes arrive via SetCursor.
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

func (h *Hub) broadcastChunkExcept(key string, except *Peer, msg interface{}) {
	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.clients))
	for p := range h.clients {
		if p != except && p.Subscribed(key) {
			peers = append(peers, p)
		}
	}
	h.mu.RUnlock()
	for _, p := range peers {
		_ = p.SendJSON(msg)
	}
}

func (h *Hub) BroadcastChunk(key string, msg interface{}) { h.broadcastChunkExcept(key, nil, msg) }

func (h *Hub) BroadcastAll(msg interface{}) {
	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.clients))
	for p := range h.clients {
		peers = append(peers, p)
	}
	h.mu.RUnlock()
	for _, p := range peers {
		_ = p.SendJSON(msg)
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
