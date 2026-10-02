package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
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
	Score  int64 // safe cells revealed
}

// MemoryStore keeps nothing across restarts: chunks are not saved, but users and
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

type TursoStore struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewTurso(baseURL, token string) *TursoStore {
	return &TursoStore{
		baseURL: strings.TrimRight(strings.Replace(baseURL, "libsql://", "https://", 1), "/"),
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *TursoStore) Init(ctx context.Context) error {
	_, err := s.pipeline(ctx, []request{execute(`CREATE TABLE IF NOT EXISTS chunks (
		world_id TEXT NOT NULL,
		chunk_x INTEGER NOT NULL,
		chunk_y INTEGER NOT NULL,
		revealed TEXT NOT NULL,
		flags TEXT NOT NULL,
		version INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY(world_id, chunk_x, chunk_y)
	)`)})
	if err != nil {
		return err
	}
	// Fails harmlessly when the column already exists.
	_, _ = s.pipeline(ctx, []request{execute(`ALTER TABLE chunks ADD COLUMN flag_owners TEXT NOT NULL DEFAULT '{}'`)})
	_, err = s.pipeline(ctx, []request{execute(`CREATE TABLE IF NOT EXISTS users (
		id TEXT PRIMARY KEY,
		pixels TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`)})
	if err != nil {
		return err
	}
	// Fail harmlessly when the columns already exist.
	_, _ = s.pipeline(ctx, []request{execute(`ALTER TABLE users ADD COLUMN name TEXT NOT NULL DEFAULT ''`)})
	_, _ = s.pipeline(ctx, []request{execute(`ALTER TABLE users ADD COLUMN score INTEGER NOT NULL DEFAULT 0`)})
	_, err = s.pipeline(ctx, []request{execute(`CREATE INDEX IF NOT EXISTS users_score ON users(score DESC)`)})
	return err
}

func (s *TursoStore) AddScores(ctx context.Context, scores map[string]int64) error {
	if len(scores) == 0 {
		return nil
	}
	now := time.Now().Unix()
	stmts := make([]stmt, 0, len(scores))
	for id, delta := range scores {
		stmts = append(stmts, *executeArgs(
			`INSERT INTO users(id, pixels, updated_at, score) VALUES(?, '', ?, ?)
			 ON CONFLICT(id) DO UPDATE SET score = score + excluded.score`,
			textArg(id), intArg(now), intArg(delta),
		).Stmt)
	}
	// All or nothing: the caller retries the whole batch on error, so a half-applied
	// batch would count some scores twice.
	return s.transaction(ctx, stmts)
}

// transaction runs stmts atomically. A pipeline keeps going after a failed statement and
// would COMMIT the rest, so use a conditional batch: COMMIT only if every statement
// succeeded, ROLLBACK otherwise.
func (s *TursoStore) transaction(ctx context.Context, stmts []stmt) error {
	ok := func(step int) *condition { return &condition{Type: "ok", Step: &step} }
	steps := []batchStep{{Stmt: &stmt{SQL: "BEGIN"}}}
	for i := range stmts {
		steps = append(steps, batchStep{Stmt: &stmts[i], Condition: ok(i)})
	}
	commit := len(steps)
	steps = append(steps,
		batchStep{Stmt: &stmt{SQL: "COMMIT"}, Condition: ok(commit - 1)},
		batchStep{Stmt: &stmt{SQL: "ROLLBACK"}, Condition: &condition{Type: "not", Cond: ok(commit)}},
	)
	raw, err := s.pipeline(ctx, []request{{Type: "batch", Batch: &batch{Steps: steps}}})
	if err != nil {
		return err
	}
	var root pipelineResponse
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	res := root.Results[0].Response.Result
	if len(res.StepResults) <= commit || string(res.StepResults[commit]) == "null" {
		return fmt.Errorf("turso: transaction rolled back: %s", firstStepError(res.StepErrors))
	}
	return nil
}

func firstStepError(errs []json.RawMessage) string {
	for _, e := range errs {
		if string(e) != "null" {
			return string(e)
		}
	}
	return "unknown error"
}

func (s *TursoStore) TopUsers(ctx context.Context, n int) ([]User, error) {
	rows, err := s.query(ctx, executeArgs("SELECT id, pixels, name, score FROM users WHERE score > 0 ORDER BY score DESC LIMIT ?", intArg(int64(n))))
	if err != nil {
		return nil, err
	}
	out := make([]User, 0, len(rows))
	for _, row := range rows {
		if len(row) >= 4 {
			out = append(out, User{ID: row[0].String(), Pixels: row[1].String(), Name: row[2].String(), Score: row[3].Int64()})
		}
	}
	return out, nil
}

func (s *TursoStore) query(ctx context.Context, req request) ([][]value, error) {
	raw, err := s.pipeline(ctx, []request{req})
	if err != nil {
		return nil, err
	}
	var root pipelineResponse
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	if len(root.Results) == 0 {
		return nil, fmt.Errorf("turso: empty response")
	}
	return root.Results[0].Response.Result.Rows, nil
}

func (s *TursoStore) LoadUsers(ctx context.Context, ids []string) (map[string]User, error) {
	out := make(map[string]User, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]value, len(ids))
	for i, id := range ids {
		args[i] = textArg(id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := s.query(ctx, executeArgs("SELECT id, pixels, name, score FROM users WHERE id IN ("+placeholders+")", args...))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if len(row) >= 4 {
			out[row[0].String()] = User{ID: row[0].String(), Pixels: row[1].String(), Name: row[2].String(), Score: row[3].Int64()}
		}
	}
	return out, nil
}

func (s *TursoStore) SaveUser(ctx context.Context, id, pixels, name string) error {
	_, err := s.pipeline(ctx, []request{executeArgs(
		`INSERT INTO users(id, pixels, name, updated_at) VALUES(?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET pixels = excluded.pixels, name = excluded.name, updated_at = excluded.updated_at`,
		textArg(id), textArg(pixels), textArg(name), intArg(time.Now().Unix()),
	)})
	return err
}

func (s *TursoStore) LoadChunk(ctx context.Context, worldID string, cx, cy int64) (ChunkRecord, bool, error) {
	raw, err := s.pipeline(ctx, []request{executeArgs(
		"SELECT revealed, flags, version, flag_owners FROM chunks WHERE world_id = ? AND chunk_x = ? AND chunk_y = ? LIMIT 1",
		textArg(worldID), intArg(cx), intArg(cy),
	)})
	if err != nil {
		return ChunkRecord{}, false, err
	}

	var root pipelineResponse
	if err := json.Unmarshal(raw, &root); err != nil {
		return ChunkRecord{}, false, err
	}
	if len(root.Results) == 0 || root.Results[0].Type != "ok" {
		return ChunkRecord{}, false, nil
	}
	rows := root.Results[0].Response.Result.Rows
	if len(rows) == 0 || len(rows[0]) < 4 {
		return ChunkRecord{}, false, nil
	}
	return ChunkRecord{
		ChunkX:   cx,
		ChunkY:   cy,
		Revealed: rows[0][0].String(),
		Flags:    rows[0][1].String(),
		Version:  rows[0][2].Int64(),
		Owners:   rows[0][3].String(),
	}, true, nil
}

func (s *TursoStore) SaveChunks(ctx context.Context, worldID string, chunks []ChunkRecord) error {
	if len(chunks) == 0 {
		return nil
	}
	// One transaction per call: with separate statements every upsert commits on its own
	// (~0.2s each on Turso), so a few dozen chunks could not be saved within the timeout.
	stmts := make([]stmt, 0, len(chunks))
	now := time.Now().Unix()
	for _, c := range chunks {
		stmts = append(stmts, *executeArgs(
			`INSERT INTO chunks(world_id, chunk_x, chunk_y, revealed, flags, flag_owners, version, updated_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(world_id, chunk_x, chunk_y) DO UPDATE SET
			   revealed = excluded.revealed,
			   flags = excluded.flags,
			   flag_owners = excluded.flag_owners,
			   version = excluded.version,
			   updated_at = excluded.updated_at
			 WHERE excluded.version >= chunks.version`,
			textArg(worldID), intArg(c.ChunkX), intArg(c.ChunkY),
			textArg(c.Revealed), textArg(c.Flags), textArg(c.Owners), intArg(c.Version), intArg(now),
		).Stmt)
	}
	return s.transaction(ctx, stmts)
}

type value struct {
	Type  string      `json:"type"`
	Value interface{} `json:"value,omitempty"`
}

func (v value) String() string {
	if s, ok := v.Value.(string); ok {
		return s
	}
	return ""
}

func (v value) Int64() int64 {
	switch n := v.Value.(type) {
	case float64:
		return int64(n)
	case string:
		var x int64
		_, _ = fmt.Sscan(n, &x)
		return x
	default:
		return 0
	}
}

type stmt struct {
	SQL  string  `json:"sql"`
	Args []value `json:"args,omitempty"`
}
type request struct {
	Type  string `json:"type"`
	Stmt  *stmt  `json:"stmt,omitempty"`
	Batch *batch `json:"batch,omitempty"`
}
type batch struct {
	Steps []batchStep `json:"steps"`
}
type batchStep struct {
	Stmt      *stmt      `json:"stmt"`
	Condition *condition `json:"condition,omitempty"`
}
type condition struct {
	Type string     `json:"type"`
	Step *int       `json:"step,omitempty"`
	Cond *condition `json:"cond,omitempty"`
}
type pipelineRequest struct {
	Requests []request `json:"requests"`
}
type queryResult struct {
	Rows        [][]value         `json:"rows"`
	StepResults []json.RawMessage `json:"step_results"` // batch only; null = step not run / failed
	StepErrors  []json.RawMessage `json:"step_errors"`
}
type responseBody struct {
	Type   string      `json:"type"`
	Result queryResult `json:"result"`
}
type pipelineResult struct {
	Type     string       `json:"type"`
	Response responseBody `json:"response"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}
type pipelineResponse struct {
	Results []pipelineResult `json:"results"`
}

func execute(sql string) request { return request{Type: "execute", Stmt: &stmt{SQL: sql}} }
func executeArgs(sql string, args ...value) request {
	return request{Type: "execute", Stmt: &stmt{SQL: sql, Args: args}}
}
func textArg(v string) value { return value{Type: "text", Value: v} }
func intArg(v int64) value   { return value{Type: "integer", Value: fmt.Sprintf("%d", v)} }

func (s *TursoStore) pipeline(ctx context.Context, reqs []request) ([]byte, error) {
	body, err := json.Marshal(pipelineRequest{Requests: reqs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v2/pipeline", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("turso http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	// SQL errors come back as HTTP 200 with a per-statement error result.
	var root pipelineResponse
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	for _, r := range root.Results {
		if r.Type == "error" {
			return nil, fmt.Errorf("turso: %s", r.Error.Message)
		}
	}
	return data, nil
}
