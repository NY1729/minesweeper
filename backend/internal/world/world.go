package world

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"

	"minesweeper/backend/internal/store"
)

const (
	ChunkSize       = int64(32)
	CellsPerChunk   = int(ChunkSize * ChunkSize)
	BitmapBytes     = CellsPerChunk / 8
	MaxRevealAction = 2048
	MineValue       = uint8(9)
	// ~1KB each; idle chunks are evicted every minute, this caps bursts in between.
	MaxLoadedChunks = 50000
)

var ErrTooManyChunks = errors.New("too many loaded chunks")

type Cell struct {
	X     int64 `json:"x"`
	Y     int64 `json:"y"`
	Value uint8 `json:"value"`
}

type ChunkSnapshot struct {
	ChunkX  int64  `json:"chunkX"`
	ChunkY  int64  `json:"chunkY"`
	Cells   []Cell `json:"cells"`
	Flags   []Flag `json:"flags"`
	Version int64  `json:"version"`
}

type Flag struct {
	Index int    `json:"i"`
	Owner string `json:"o"`
}

type chunk struct {
	revealed []byte
	flags    []byte
	owners   map[int]string
	version  int64
	dirty    bool
}

type Manager struct {
	worldID      string
	seed         []byte
	minePermille uint64
	store        store.Store
	mu           sync.RWMutex
	chunks       map[string]*chunk
	opMu         sync.Mutex
}

func NewManager(worldID, seed string, minePermille uint64, st store.Store) *Manager {
	return &Manager{worldID: worldID, seed: []byte(seed), minePermille: minePermille, store: st, chunks: make(map[string]*chunk)}
}

func key(cx, cy int64) string { return fmt.Sprintf("%d:%d", cx, cy) }

func floorDiv(a, b int64) int64 {
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q
}

func mod(a, b int64) int64 {
	r := a % b
	if r < 0 {
		r += b
	}
	return r
}

func toChunk(x, y int64) (cx, cy int64, idx int) {
	cx, cy = floorDiv(x, ChunkSize), floorDiv(y, ChunkSize)
	lx, ly := mod(x, ChunkSize), mod(y, ChunkSize)
	idx = int(ly*ChunkSize + lx)
	return
}

func (m *Manager) MineAt(x, y int64) bool {
	mac := hmac.New(sha256.New, m.seed)
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], uint64(x))
	binary.LittleEndian.PutUint64(buf[8:], uint64(y))
	_, _ = mac.Write(buf[:])
	sum := mac.Sum(nil)
	return binary.LittleEndian.Uint64(sum[:8])%1000 < m.minePermille
}

func (m *Manager) ValueAt(x, y int64) uint8 {
	if m.MineAt(x, y) {
		return MineValue
	}
	var n uint8
	for dy := int64(-1); dy <= 1; dy++ {
		for dx := int64(-1); dx <= 1; dx++ {
			if dx == 0 && dy == 0 {
				continue
			}
			if m.MineAt(x+dx, y+dy) {
				n++
			}
		}
	}
	return n
}

func bitGet(b []byte, idx int) bool { return b[idx>>3]&(1<<uint(idx&7)) != 0 }

func bitSet(b []byte, idx int, on bool) {
	mask := byte(1 << uint(idx&7))
	if on {
		b[idx>>3] |= mask
	} else {
		b[idx>>3] &^= mask
	}
}

func (m *Manager) getChunk(ctx context.Context, cx, cy int64) (*chunk, error) {
	k := key(cx, cy)
	m.mu.RLock()
	if c := m.chunks[k]; c != nil {
		m.mu.RUnlock()
		return c, nil
	}
	m.mu.RUnlock()

	m.mu.RLock()
	full := len(m.chunks) >= MaxLoadedChunks
	m.mu.RUnlock()
	if full {
		return nil, ErrTooManyChunks
	}

	rec, ok, err := m.store.LoadChunk(ctx, m.worldID, cx, cy)
	if err != nil {
		return nil, err
	}

	c := &chunk{revealed: make([]byte, BitmapBytes), flags: make([]byte, BitmapBytes), owners: map[int]string{}}
	if ok {
		if decoded, err := hex.DecodeString(rec.Revealed); err == nil && len(decoded) == BitmapBytes {
			copy(c.revealed, decoded)
		}
		if decoded, err := hex.DecodeString(rec.Flags); err == nil && len(decoded) == BitmapBytes {
			copy(c.flags, decoded)
		}
		_ = json.Unmarshal([]byte(rec.Owners), &c.owners)
		if c.owners == nil {
			c.owners = map[int]string{}
		}
		c.version = rec.Version
	}

	m.mu.Lock()
	if existing := m.chunks[k]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	m.chunks[k] = c
	m.mu.Unlock()
	return c, nil
}

func (m *Manager) Snapshot(ctx context.Context, cx, cy int64) (ChunkSnapshot, error) {
	c, err := m.getChunk(ctx, cx, cy)
	if err != nil {
		return ChunkSnapshot{}, err
	}

	m.mu.RLock()
	revealed := append([]byte(nil), c.revealed...)
	flags := append([]byte(nil), c.flags...)
	owners := make(map[int]string, len(c.owners))
	for k, v := range c.owners {
		owners[k] = v
	}
	version := c.version
	m.mu.RUnlock()

	out := ChunkSnapshot{ChunkX: cx, ChunkY: cy, Version: version}
	for idx := 0; idx < CellsPerChunk; idx++ {
		lx := int64(idx) % ChunkSize
		ly := int64(idx) / ChunkSize
		x, y := cx*ChunkSize+lx, cy*ChunkSize+ly
		if bitGet(revealed, idx) {
			out.Cells = append(out.Cells, Cell{X: x, Y: y, Value: m.ValueAt(x, y)})
		}
		if bitGet(flags, idx) {
			out.Flags = append(out.Flags, Flag{Index: idx, Owner: owners[idx]})
		}
	}
	return out, nil
}

// FlagResult describes a ToggleFlag call.
type FlagResult struct {
	On      bool   // flag state after the call
	Owner   string // owner of the flag if On
	ChunkX  int64
	ChunkY  int64
	Version int64
	Changed bool
}

// ToggleFlag places a flag owned by owner, or removes it if owner placed it.
// Flags from before ownership existed (owner "") can be removed by anyone.
func (m *Manager) ToggleFlag(ctx context.Context, x, y int64, owner string) (FlagResult, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	cx, cy, idx := toChunk(x, y)
	c, err := m.getChunk(ctx, cx, cy)
	if err != nil {
		return FlagResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	res := FlagResult{ChunkX: cx, ChunkY: cy, Version: c.version, On: bitGet(c.flags, idx), Owner: c.owners[idx]}
	if bitGet(c.revealed, idx) {
		return res, nil
	}
	if res.On && res.Owner != "" && res.Owner != owner {
		return res, nil
	}

	res.On = !res.On
	bitSet(c.flags, idx, res.On)
	if res.On {
		c.owners[idx] = owner
	} else {
		delete(c.owners, idx)
	}
	c.version++
	c.dirty = true
	res.Version, res.Owner, res.Changed = c.version, c.owners[idx], true
	return res, nil
}

func (m *Manager) Reveal(ctx context.Context, startX, startY int64) ([]Cell, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	type point struct{ x, y int64 }
	queue := []point{{startX, startY}}
	seen := make(map[point]struct{}, 128)
	updates := make([]Cell, 0, 128)

	for len(queue) > 0 && len(updates) < MaxRevealAction {
		p := queue[0]
		queue = queue[1:]
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}

		cx, cy, idx := toChunk(p.x, p.y)
		c, err := m.getChunk(ctx, cx, cy)
		if err != nil {
			return updates, err
		}

		m.mu.Lock()
		if bitGet(c.revealed, idx) || bitGet(c.flags, idx) {
			m.mu.Unlock()
			continue
		}
		value := m.ValueAt(p.x, p.y)
		bitSet(c.revealed, idx, true)
		c.version++
		c.dirty = true
		m.mu.Unlock()

		updates = append(updates, Cell{X: p.x, Y: p.y, Value: value})
		if value != 0 {
			continue
		}

		for dy := int64(-1); dy <= 1; dy++ {
			for dx := int64(-1); dx <= 1; dx++ {
				if dx == 0 && dy == 0 {
					continue
				}
				queue = append(queue, point{p.x + dx, p.y + dy})
			}
		}
	}
	return updates, nil
}

func (m *Manager) DirtySnapshots() []store.ChunkRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]store.ChunkRecord, 0)
	for k, c := range m.chunks {
		if !c.dirty {
			continue
		}
		var cx, cy int64
		_, _ = fmt.Sscanf(k, "%d:%d", &cx, &cy)
		owners, _ := json.Marshal(c.owners)
		out = append(out, store.ChunkRecord{
			ChunkX: cx, ChunkY: cy,
			Revealed: hex.EncodeToString(c.revealed),
			Flags:    hex.EncodeToString(c.flags),
			Owners:   string(owners),
			Version:  c.version,
		})
		c.dirty = false
	}
	return out
}

func (m *Manager) MarkDirty(records []store.ChunkRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range records {
		if c := m.chunks[key(r.ChunkX, r.ChunkY)]; c != nil {
			c.dirty = true
		}
	}
}

// Evict drops saved chunks for which keep returns false and reports how many.
// Holds opMu so no Reveal/ToggleFlag is mid-way through a chunk being dropped.
func (m *Manager) Evict(keep func(cx, cy int64) bool) int {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, c := range m.chunks {
		if c.dirty {
			continue
		}
		var cx, cy int64
		_, _ = fmt.Sscanf(k, "%d:%d", &cx, &cy)
		if keep(cx, cy) {
			continue
		}
		delete(m.chunks, k)
		n++
	}
	return n
}

// MinePenalty is the score lost for opening a mine. It scales with density so that
// clicking at random always loses points: expected gain (1-p) - p*penalty < 0.
func (m *Manager) MinePenalty() int64 {
	p := float64(m.minePermille) / 1000
	if p <= 0 {
		return 0
	}
	return max(10, int64(math.Ceil(1.5*(1-p)/p)))
}

// ScoreDelta is the score change for a click at (x, y) that revealed cells:
// +1 for opening a safe cell however far it floods (paying per flooded cell would make
// random clicks hugely profitable), -MinePenalty for opening a mine, 0 if nothing opened.
func (m *Manager) ScoreDelta(x, y int64, cells []Cell) int64 {
	if len(cells) == 0 || cells[0].X != x || cells[0].Y != y {
		return 0
	}
	if cells[0].Value == MineValue {
		return -m.MinePenalty()
	}
	return 1
}
