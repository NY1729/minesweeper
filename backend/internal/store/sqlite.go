package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go driver: no cgo, so the static distroless image keeps working
)

// SQLiteStore keeps everything in one local file. There is no network in the path, so a
// slow or unreachable remote database can no longer stop the game or its start-up.
type SQLiteStore struct{ db *sql.DB }

// OpenSQLite opens (and creates if needed) the database file at path.
func OpenSQLite(path string) (*SQLiteStore, error) {
	// WAL: readers never wait for the writer; NORMAL sync is safe with WAL (a power cut can
	// lose the last moments, never corrupt the file).
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: writes are serialised by SQLite anyway, and this avoids "database is
	// locked" between our own goroutines. Local queries take microseconds.
	db.SetMaxOpenConns(1)
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) Init(ctx context.Context) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS chunks (
			world_id TEXT NOT NULL,
			chunk_x INTEGER NOT NULL,
			chunk_y INTEGER NOT NULL,
			revealed TEXT NOT NULL,
			flags TEXT NOT NULL,
			flag_owners TEXT NOT NULL DEFAULT '{}',
			version INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY(world_id, chunk_x, chunk_y)
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			pixels TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			score INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS users_score ON users(score DESC)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) LoadChunk(ctx context.Context, worldID string, cx, cy int64) (ChunkRecord, bool, error) {
	rec := ChunkRecord{ChunkX: cx, ChunkY: cy}
	err := s.db.QueryRowContext(ctx,
		"SELECT revealed, flags, version, flag_owners FROM chunks WHERE world_id = ? AND chunk_x = ? AND chunk_y = ?",
		worldID, cx, cy).Scan(&rec.Revealed, &rec.Flags, &rec.Version, &rec.Owners)
	if errors.Is(err, sql.ErrNoRows) {
		return ChunkRecord{}, false, nil
	}
	if err != nil {
		return ChunkRecord{}, false, err
	}
	return rec, true, nil
}

// SaveChunks writes all chunks in one transaction (one commit, all or nothing). A chunk is
// only overwritten by the same or a newer version.
func (s *SQLiteStore) SaveChunks(ctx context.Context, worldID string, chunks []ChunkRecord) error {
	if len(chunks) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO chunks(world_id, chunk_x, chunk_y, revealed, flags, flag_owners, version, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(world_id, chunk_x, chunk_y) DO UPDATE SET
			  revealed = excluded.revealed,
			  flags = excluded.flags,
			  flag_owners = excluded.flag_owners,
			  version = excluded.version,
			  updated_at = excluded.updated_at
			WHERE excluded.version >= chunks.version`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		now := time.Now().Unix()
		for _, c := range chunks {
			if _, err := stmt.ExecContext(ctx, worldID, c.ChunkX, c.ChunkY, c.Revealed, c.Flags, c.Owners, c.Version, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLiteStore) LoadUsers(ctx context.Context, ids []string) (map[string]User, error) {
	out := make(map[string]User, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, pixels, name, score FROM users WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Pixels, &u.Name, &u.Score); err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SaveUser(ctx context.Context, id, pixels, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id, pixels, name, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET pixels = excluded.pixels, name = excluded.name, updated_at = excluded.updated_at`,
		id, pixels, name, time.Now().Unix())
	return err
}

// AddScores applies all deltas in one transaction.
func (s *SQLiteStore) AddScores(ctx context.Context, scores map[string]int64) error {
	if len(scores) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		for id, delta := range scores {
			if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, pixels, updated_at, score) VALUES(?, '', ?, ?)
				ON CONFLICT(id) DO UPDATE SET score = score + excluded.score`, id, now, delta); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLiteStore) TopUsers(ctx context.Context, n int) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, pixels, name, score FROM users WHERE score > 0 ORDER BY score DESC LIMIT ?", n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Pixels, &u.Name, &u.Score); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ImportUsers upserts whole user rows, score included (used once, to move data over from Turso).
func (s *SQLiteStore) ImportUsers(ctx context.Context, users []User) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		for _, u := range users {
			if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, pixels, name, score, updated_at) VALUES(?, ?, ?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET pixels = excluded.pixels, name = excluded.name, score = excluded.score, updated_at = excluded.updated_at`,
				u.ID, u.Pixels, u.Name, u.Score, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLiteStore) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sqlite: %w", err)
	}
	return tx.Commit()
}
