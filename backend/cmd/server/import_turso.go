package main

// One-time move of the old data from Turso into the local SQLite file (`-import-turso`).
// Safe to repeat: chunks are only replaced by the same or a newer version, and users are
// replaced rather than added to. Delete this file once the data has been moved.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"minesweeper/backend/internal/config"
	"minesweeper/backend/internal/store"
)

type tursoValue struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

func (v tursoValue) str() string {
	if s, ok := v.Value.(string); ok {
		return s
	}
	return ""
}

func (v tursoValue) int() int64 {
	switch n := v.Value.(type) {
	case string:
		x, _ := strconv.ParseInt(n, 10, 64)
		return x
	case float64:
		return int64(n)
	}
	return 0
}

// tursoQuery runs one SELECT through Turso's HTTP API and returns its rows.
func tursoQuery(ctx context.Context, cfg config.Config, sql string, args ...tursoValue) ([][]tursoValue, error) {
	type stmt struct {
		SQL  string       `json:"sql"`
		Args []tursoValue `json:"args,omitempty"`
	}
	body, _ := json.Marshal(map[string]any{"requests": []map[string]any{{"type": "execute", "stmt": stmt{sql, args}}}})
	url := strings.TrimRight(strings.Replace(cfg.TursoDatabaseURL, "libsql://", "https://", 1), "/") + "/v2/pipeline"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.TursoAuthToken)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out struct {
		Results []struct {
			Type     string `json:"type"`
			Response struct {
				Result struct {
					Rows [][]tursoValue `json:"rows"`
				} `json:"result"`
			} `json:"response"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"results"`
	}
	if resp.StatusCode/100 != 2 || json.Unmarshal(data, &out) != nil || len(out.Results) == 0 {
		return nil, fmt.Errorf("turso http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out.Results[0].Type != "ok" {
		return nil, fmt.Errorf("turso: %s", out.Results[0].Error.Message)
	}
	return out.Results[0].Response.Result.Rows, nil
}

func runImportTurso(cfg config.Config, db *store.SQLiteStore) {
	if cfg.TursoDatabaseURL == "" {
		log.Fatal("TURSO_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := db.Init(ctx); err != nil {
		log.Fatalf("init database: %v", err)
	}

	const page = 500
	chunks := 0
	for offset := 0; ; offset += page {
		rows, err := tursoQuery(ctx, cfg,
			"SELECT chunk_x, chunk_y, revealed, flags, flag_owners, version FROM chunks WHERE world_id = ? ORDER BY chunk_x, chunk_y LIMIT ? OFFSET ?",
			tursoValue{"text", cfg.WorldID}, tursoValue{"integer", strconv.Itoa(page)}, tursoValue{"integer", strconv.Itoa(offset)})
		if err != nil {
			log.Fatalf("read chunks from Turso: %v", err)
		}
		recs := make([]store.ChunkRecord, 0, len(rows))
		for _, r := range rows {
			recs = append(recs, store.ChunkRecord{ChunkX: r[0].int(), ChunkY: r[1].int(), Revealed: r[2].str(), Flags: r[3].str(), Owners: r[4].str(), Version: r[5].int()})
		}
		if err := db.SaveChunks(ctx, cfg.WorldID, recs); err != nil {
			log.Fatalf("write chunks: %v", err)
		}
		chunks += len(recs)
		if len(rows) < page {
			break
		}
	}

	users := 0
	for offset := 0; ; offset += page {
		rows, err := tursoQuery(ctx, cfg, "SELECT id, pixels, name, score FROM users ORDER BY id LIMIT ? OFFSET ?",
			tursoValue{"integer", strconv.Itoa(page)}, tursoValue{"integer", strconv.Itoa(offset)})
		if err != nil {
			log.Fatalf("read users from Turso: %v", err)
		}
		list := make([]store.User, 0, len(rows))
		for _, r := range rows {
			list = append(list, store.User{ID: r[0].str(), Pixels: r[1].str(), Name: r[2].str(), Score: r[3].int()})
		}
		if err := db.ImportUsers(ctx, list); err != nil {
			log.Fatalf("write users: %v", err)
		}
		users += len(list)
		if len(rows) < page {
			break
		}
	}
	fmt.Printf("imported %d chunks (world %q) and %d users into %s\n", chunks, cfg.WorldID, users, cfg.DatabasePath)
}
