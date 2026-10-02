package store

import (
	"context"
	"sort"
	"sync"
)

type ChunkRecord struct {
	ChunkX   int64
	ChunkY   int64
	Revealed string
	Flags    string
	Owners   string
	Version  int64
}

type Store interface {
	Init(context.Context) error
	LoadChunk(context.Context, string, int64, int64) (ChunkRecord, bool, error)
	SaveChunks(context.Context, string, []ChunkRecord) error
	LoadUsers(context.Context, []string) (map[string]User, error)
	SaveUser(ctx context.Context, id, pixels, name string) error
	AddScores(context.Context, map[string]int64) error
	TopUsers(ctx context.Context, n int) ([]User, error)
}

type User struct {
	ID     string
	Pixels string
	Name   string
	Score  int64 // safe cells opened minus penalties for mines
}

// MemoryStore keeps nothing across restarts (development, tests): chunks are not saved, but users and
// scores are held in memory so profiles and the ranking still work.
type MemoryStore struct {
	mu    sync.Mutex
	users map[string]User
}

func (*MemoryStore) Init(context.Context) error { return nil }
func (*MemoryStore) LoadChunk(context.Context, string, int64, int64) (ChunkRecord, bool, error) {
	return ChunkRecord{}, false, nil
}
func (*MemoryStore) SaveChunks(context.Context, string, []ChunkRecord) error { return nil }

func (m *MemoryStore) LoadUsers(_ context.Context, ids []string) (map[string]User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]User, len(ids))
	for _, id := range ids {
		if u, ok := m.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (m *MemoryStore) SaveUser(_ context.Context, id, pixels, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.users == nil {
		m.users = map[string]User{}
	}
	u := m.users[id]
	u.ID, u.Pixels, u.Name = id, pixels, name
	m.users[id] = u
	return nil
}

func (m *MemoryStore) AddScores(_ context.Context, scores map[string]int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.users == nil {
		m.users = map[string]User{}
	}
	for id, d := range scores {
		u := m.users[id]
		u.ID = id
		u.Score += d
		m.users[id] = u
	}
	return nil
}

func (m *MemoryStore) TopUsers(_ context.Context, n int) ([]User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []User
	for _, u := range m.users {
		if u.Score > 0 {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}
