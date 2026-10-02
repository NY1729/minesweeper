package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr             string
	WorldID          string
	WorldSeed        string
	MinePermille     uint64
	DatabasePath     string // SQLite file; empty = keep nothing across restarts (development)
	TursoDatabaseURL string // only read by `-import-turso`, to move old data over once
	TursoAuthToken   string
	AllowedOrigins   []string
	FlushInterval    time.Duration
	MaxConnsPerIP    int // simultaneous websockets from one client IP (people behind one NAT share it)
}

func Load() Config {
	port := getenv("PORT", "8080")
	minePermille := uint64(160)
	if v, err := strconv.ParseUint(getenv("MINE_PERMILLE", "160"), 10, 64); err == nil && v <= 500 {
		minePermille = v
	}
	flushSeconds := 2
	if v, err := strconv.Atoi(getenv("FLUSH_INTERVAL_SECONDS", "2")); err == nil && v > 0 {
		flushSeconds = v
	}
	maxConns := 20
	if v, err := strconv.Atoi(getenv("MAX_CONNS_PER_IP", "20")); err == nil && v > 0 {
		maxConns = v
	}
	origins := strings.Split(getenv("ALLOWED_ORIGINS", "*"), ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	return Config{
		Addr:             ":" + port,
		WorldID:          getenv("WORLD_ID", "main"),
		WorldSeed:        getenv("WORLD_SEED", "change-me-in-production"),
		MinePermille:     minePermille,
		DatabasePath:     os.Getenv("DATABASE_PATH"),
		TursoDatabaseURL: strings.TrimRight(os.Getenv("TURSO_DATABASE_URL"), "/"),
		TursoAuthToken:   os.Getenv("TURSO_AUTH_TOKEN"),
		AllowedOrigins:   origins,
		FlushInterval:    time.Duration(flushSeconds) * time.Second,
		MaxConnsPerIP:    maxConns,
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
