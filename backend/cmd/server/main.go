package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"minesweeper/backend/internal/config"
	"minesweeper/backend/internal/realtime"
	"minesweeper/backend/internal/store"
	"minesweeper/backend/internal/world"
)

type wsClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *wsClient) SendJSON(v interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteJSON(v)
}

// SendBytes sends an already encoded JSON message (used by broadcasts, which encode once).
func (c *wsClient) SendBytes(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

type server struct {
	cfg      config.Config
	world    *world.Manager
	store    store.Store
	hub      *realtime.Hub
	upgrader websocket.Upgrader

	usersMu       sync.Mutex
	users         map[string]*store.User // public id -> profile; Score includes pendingScores
	pendingScores map[string]int64       // not yet written to the database

	rankMu     sync.Mutex
	rankTop    []rankEntry // last ranking pushed to clients
	rankLoaded bool
	scoreDirty atomic.Bool // a score changed since the last ranking push

	rates rateLimits

	pingEvery, readTimeout time.Duration

	connsMu sync.Mutex
	conns   map[string]int // client ip -> open websockets
}

// A browser answers pings by itself. A peer that stays silent for readTimeout is gone (phone
// asleep, network switched): drop it, or it keeps holding a connection slot of its IP for a long time.
const (
	defaultPingEvery   = 25 * time.Second
	defaultReadTimeout = 70 * time.Second
)

// bucket is a token bucket: rate tokens/sec, holding at most burst.
// Only touched by its connection's read loop, so no locking.
type bucket struct {
	rate, burst, tokens float64
	last                time.Time
}

func newBucket(rate, burst float64) *bucket { return &bucket{rate: rate, burst: burst, tokens: burst} }

func (b *bucket) allow(now time.Time) bool {
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLimits holds token buckets keyed by user id or IP. Unlike the per-connection
// buckets they survive reconnects and can't be multiplied by opening more connections.
type rateLimits struct {
	mu sync.Mutex
	m  map[string]*bucket
}

func (r *rateLimits) allow(key string, rate, burst float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[string]*bucket)
	}
	b := r.m[key]
	if b == nil {
		b = newBucket(rate, burst)
		r.m[key] = b
	}
	return b.allow(time.Now())
}

// sweep forgets buckets idle for 2 minutes (they would have refilled anyway).
func (r *rateLimits) sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, b := range r.m {
		if time.Since(b.last) > 2*time.Minute {
			delete(r.m, k)
		}
	}
}

// allowShared applies the per-user and per-IP limits for actions that change the world or
// cost a database write. A human tops out around 5 clicks/s; the IP limit leaves room for
// a few people behind one NAT.
func (s *server) allowShared(typ, userID, ip string) bool {
	switch typ {
	case "reveal", "flag":
		return s.rates.allow("act:"+userID, 6, 12) && s.rates.allow("act-ip:"+ip, 15, 30)
	case "setProfile":
		return s.rates.allow("prof:"+userID, 0.1, 1)
	}
	return true
}

// clientIP trusts CF-Connecting-IP: the backend is only reachable through Cloudflare Tunnel.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// A flag is 16x16 pixels in one of two formats, told apart by length:
//   - 1024 chars: per pixel "a r g b", one hex digit each (RGB 16 levels per channel; alpha is 0 or f).
//   - 256 chars (legacy): one base-36 digit per pixel, a palette index 0-31.
//
// Flags saved in the legacy format stay valid; the client converts them when they are edited.
var pixelsRe = regexp.MustCompile(`^(?:(?:[0f][0-9a-f]{3}){256}|[0-9a-v]{256})$`)
var userIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

const maxNameRunes = 16

// cleanName trims and validates a display name; ok=false rejects it. Empty is allowed.
func cleanName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > maxNameRunes {
		return "", false
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return "", false
		}
	}
	return name, true
}

// profile is a user as sent to clients.
type profile struct {
	Pixels string `json:"p"`
	Name   string `json:"n"`
}

type rankEntry struct {
	ID    string `json:"id"`
	Name  string `json:"n"`
	Score int64  `json:"score"`
}

type incoming struct {
	Type   string       `json:"type"`
	X      int64        `json:"x,omitempty"`
	Y      int64        `json:"y,omitempty"`
	Chunks []chunkCoord `json:"chunks,omitempty"`
	Secret string       `json:"secret,omitempty"`
	Pixels string       `json:"pixels,omitempty"`
	IDs    []string     `json:"ids,omitempty"`
	Name   string       `json:"name,omitempty"`
	PX     float64      `json:"px,omitempty"`
	PY     float64      `json:"py,omitempty"`
	Hide   bool         `json:"hide,omitempty"`
}

type chunkCoord struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

func main() {
	cfg := config.Load()

	importTurso := flag.Bool("import-turso", false, "copy the users and chunks from the old Turso database into DATABASE_PATH, then exit (needs TURSO_DATABASE_URL and TURSO_AUTH_TOKEN)")
	flag.Parse()

	var st store.Store = &store.MemoryStore{}
	if cfg.DatabasePath != "" {
		db, err := store.OpenSQLite(cfg.DatabasePath)
		if err != nil {
			log.Fatalf("open database %s: %v", cfg.DatabasePath, err)
		}
		defer db.Close()
		st = db
		if *importTurso {
			runImportTurso(cfg, db)
			return
		}
	} else if *importTurso {
		log.Fatal("DATABASE_PATH must be set for -import-turso")
	} else {
		log.Print("DATABASE_PATH is empty; using non-persistent memory store")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := st.Init(ctx); err != nil {
		cancel()
		log.Fatalf("store init: %v", err)
	}
	cancel()

	s := &server{
		cfg:           cfg,
		world:         world.NewManager(cfg.WorldID, cfg.WorldSeed, cfg.MinePermille, st),
		store:         st,
		hub:           realtime.NewHub(),
		users:         make(map[string]*store.User),
		pendingScores: make(map[string]int64),
		conns:         make(map[string]int),
		pingEvery:     defaultPingEvery,
		readTimeout:   defaultReadTimeout,
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     s.originAllowed,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.health)
	mux.HandleFunc("/api/ws", s.ws)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.cors(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.flushLoop(rootCtx)
	go s.hub.CursorLoop(rootCtx, 100*time.Millisecond) // cursor changes go out in batches, 10 times a second

	go func() {
		log.Printf("minesweeper server listening on %s", cfg.Addr)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-rootCtx.Done()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	s.flushAll(shutdownCtx) // everything is on disk before the database closes (deferred above)
	_ = httpServer.Shutdown(shutdownCtx)
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	for _, allowed := range s.cfg.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}
	return false
}

func (s *server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if s.originAllowed(r) {
			if contains(s.cfg.AllowedOrigins, "*") {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func contains(items []string, target string) bool {
	for _, x := range items {
		if strings.TrimSpace(x) == target {
			return true
		}
	}
	return false
}

func (s *server) ws(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	s.connsMu.Lock()
	if s.conns[ip] >= s.cfg.MaxConnsPerIP {
		s.connsMu.Unlock()
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	s.conns[ip]++
	s.connsMu.Unlock()
	defer func() {
		s.connsMu.Lock()
		if s.conns[ip]--; s.conns[ip] <= 0 {
			delete(s.conns, ip)
		}
		s.connsMu.Unlock()
	}()

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &wsClient{conn: conn}
	peer := realtime.NewPeer(client)
	s.hub.Add(peer)

	defer func() {
		s.hub.Remove(peer)
		_ = conn.Close()
	}()

	_ = client.SendJSON(map[string]interface{}{
		"type": "hello", "chunkSize": world.ChunkSize, "worldId": s.cfg.WorldID,
	})

	conn.SetReadLimit(64 << 10)
	conn.SetReadDeadline(time.Now().Add(s.readTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(s.readTimeout)) })
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(s.pingEvery)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				// WriteControl is safe to call alongside the other writers
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					return
				}
			}
		}
	}()
	userID := ""

	// Over-limit messages are dropped; 100 drops within 10s means a bot, so disconnect.
	actions := newBucket(10, 20) // reveal + flag share one budget
	limits := map[string]*bucket{
		"reveal":     actions,
		"flag":       actions,
		"subscribe":  newBucket(5, 10),
		"setProfile": newBucket(0.1, 1),
		"ranking":    newBucket(1, 3),
		"users":      newBucket(5, 10),
		"auth":       newBucket(1, 3),
		"ping":       newBucket(1, 5),
		"cursor":     newBucket(12, 20), // client sends at most 10/s
	}
	drops, dropWindow := 0, time.Now()

	for {
		var msg incoming
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(s.readTimeout))
		now := time.Now()
		needsAuth := msg.Type == "reveal" || msg.Type == "flag" || msg.Type == "setProfile"
		if needsAuth && userID == "" {
			continue
		}
		if b := limits[msg.Type]; b == nil || !b.allow(now) || !s.allowShared(msg.Type, userID, ip) {
			if now.Sub(dropWindow) > 10*time.Second {
				drops, dropWindow = 0, now
			}
			if drops++; drops >= 100 {
				log.Printf("disconnecting %s: rate limit", ip)
				return
			}
			continue
		}
		switch msg.Type {
		case "subscribe":
			s.handleSubscribe(r.Context(), peer, msg.Chunks)
		case "reveal":
			if delta := s.handleReveal(r.Context(), msg.X, msg.Y, userID); delta != 0 {
				// Own score updates instantly on every tab/device of this user; the shared ranking follows within ~2s.
				s.hub.SendToUser(userID, map[string]interface{}{"type": "score", "me": s.userScore(userID)})
			}
		case "auth":
			// The secret stays in the browser; only its hash is ever shown to others.
			// One identity per connection: switching would let a client mint users at will.
			if userID != "" || len(msg.Secret) < 16 || len(msg.Secret) > 128 {
				continue
			}
			sum := sha256.Sum256([]byte(msg.Secret))
			userID = hex.EncodeToString(sum[:8])
			s.hub.Bind(peer, userID)
			users, err := s.loadUsers(r.Context(), []string{userID})
			if err != nil {
				// Don't reply "no flag yet": the client would overwrite the saved one.
				return
			}
			u := users[userID]
			s.usersMu.Lock()
			if s.users[userID] == nil { // first visit: track the score from the start
				s.users[userID] = &store.User{ID: userID}
			}
			s.usersMu.Unlock()
			_ = client.SendJSON(map[string]interface{}{"type": "me", "id": userID, "pixels": u.Pixels, "name": u.Name, "score": s.userScore(userID)})
		case "setProfile":
			name, ok := cleanName(msg.Name)
			if !ok || !pixelsRe.MatchString(msg.Pixels) {
				continue
			}
			if err := s.store.SaveUser(r.Context(), userID, msg.Pixels, name); err != nil {
				log.Printf("save user: %v", err)
				continue
			}
			s.usersMu.Lock()
			if u := s.users[userID]; u != nil {
				u.Pixels, u.Name = msg.Pixels, name
			} else {
				s.users[userID] = &store.User{ID: userID, Pixels: msg.Pixels, Name: name}
			}
			s.usersMu.Unlock()
			s.hub.BroadcastAll(map[string]interface{}{"type": "users", "users": map[string]profile{userID: {msg.Pixels, name}}})
		case "users":
			if len(msg.IDs) > 64 {
				msg.IDs = msg.IDs[:64]
			}
			users, _ := s.loadUsers(r.Context(), msg.IDs)
			out := make(map[string]profile, len(users))
			for id, u := range users {
				out[id] = profile{u.Pixels, u.Name}
			}
			_ = client.SendJSON(map[string]interface{}{"type": "users", "users": out})
		case "ranking":
			_ = client.SendJSON(map[string]interface{}{"type": "ranking", "top": s.ranking(r.Context())})
		case "flag":
			s.handleFlag(r.Context(), msg.X, msg.Y, userID)
		case "cursor":
			if userID == "" {
				continue
			}
			chunk := realtime.ChunkKey(floorDiv(int64(math.Floor(msg.PX)), world.ChunkSize), floorDiv(int64(math.Floor(msg.PY)), world.ChunkSize))
			s.hub.SetCursor(peer, userID, msg.PX, msg.PY, chunk, !msg.Hide)
		case "ping":
			_ = client.SendJSON(map[string]interface{}{"type": "pong"})
		}
	}
}

func (s *server) handleSubscribe(ctx context.Context, peer *realtime.Peer, chunks []chunkCoord) {
	if len(chunks) > 64 {
		chunks = chunks[:64]
	}
	keys := make([]string, 0, len(chunks))
	for _, c := range chunks {
		keys = append(keys, realtime.ChunkKey(c.X, c.Y))
	}
	peer.SetSubscriptions(keys)
	_ = peer.SendJSON(map[string]interface{}{"type": "cursors", "cursors": s.hub.CursorsFor(peer)})

	// Read the chunks from the store in parallel (one after another took ~28 round trips).
	coords := make([][2]int64, 0, len(chunks))
	for _, c := range chunks {
		coords = append(coords, [2]int64{c.X, c.Y})
	}
	s.world.Prefetch(ctx, coords, 8)

	for _, c := range chunks {
		snap, err := s.world.Snapshot(ctx, c.X, c.Y)
		if err != nil {
			continue
		}
		_ = peer.SendJSON(map[string]interface{}{"type": "chunk", "data": snap})
	}
}

// handleReveal opens cells for userID and returns the score change (see world.ScoreDelta).
// Flags score nothing, so a score change never hints whether a flag is right.
func (s *server) handleReveal(ctx context.Context, x, y int64, userID string) int64 {
	cells, _ := s.world.Reveal(ctx, x, y) // cells revealed before an error are real and still broadcast
	if len(cells) == 0 {
		return 0
	}
	delta := s.world.ScoreDelta(x, y, cells)
	if delta != 0 {
		s.addScore(userID, delta)
	}

	grouped := make(map[string][]world.Cell)
	for _, cell := range cells {
		cx := floorDiv(cell.X, world.ChunkSize)
		cy := floorDiv(cell.Y, world.ChunkSize)
		k := realtime.ChunkKey(cx, cy)
		grouped[k] = append(grouped[k], cell)
	}

	for k, batch := range grouped {
		s.hub.BroadcastChunk(k, map[string]interface{}{"type": "reveal", "cells": batch})
	}
	return delta
}

func (s *server) handleFlag(ctx context.Context, x, y int64, owner string) {
	res, err := s.world.ToggleFlag(ctx, x, y, owner)
	if err != nil || !res.Changed {
		return
	}
	s.hub.BroadcastChunk(realtime.ChunkKey(res.ChunkX, res.ChunkY), map[string]interface{}{
		"type": "flag", "x": x, "y": y, "on": res.On, "owner": res.Owner, "version": res.Version,
	})
}

// addScore changes a user's score; the database is updated by the next flush.
func (s *server) addScore(userID string, delta int64) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	s.pendingScores[userID] += delta
	if u := s.users[userID]; u != nil {
		u.Score += delta
	}
	s.scoreDirty.Store(true)
}

func (s *server) userScore(userID string) int64 {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	if u := s.users[userID]; u != nil {
		return u.Score
	}
	return 0
}

// ranking returns the last top 10 pushed to clients (loaded once on first use).
func (s *server) ranking(ctx context.Context) []rankEntry {
	s.rankMu.Lock()
	loaded := s.rankLoaded
	s.rankMu.Unlock()
	if !loaded {
		s.pushRanking(ctx, false)
	}
	s.rankMu.Lock()
	defer s.rankMu.Unlock()
	return append([]rankEntry{}, s.rankTop...)
}

// pushRanking reads the top 10 from the store and, if it changed, sends it to everyone.
// Called right after each flush, so the store already holds the latest scores.
func (s *server) pushRanking(ctx context.Context, broadcast bool) {
	top, err := s.store.TopUsers(ctx, 10)
	if err != nil {
		log.Printf("ranking: %v", err)
		s.scoreDirty.Store(true) // retry next tick
		return
	}
	next := make([]rankEntry, len(top))
	for i, u := range top {
		next[i] = rankEntry{u.ID, u.Name, u.Score}
	}
	s.rankMu.Lock()
	changed := !s.rankLoaded || !slices.Equal(next, s.rankTop)
	s.rankTop, s.rankLoaded = next, true
	s.rankMu.Unlock()
	if changed && broadcast {
		s.hub.BroadcastAll(map[string]interface{}{"type": "ranking", "top": next})
	}
}

// loadUsers returns profiles for known ids, from memory or the database. Unknown ids are omitted.
// ponytail: cache never evicts; fine until there are millions of users.
func (s *server) loadUsers(ctx context.Context, ids []string) (map[string]store.User, error) {
	out := make(map[string]store.User, len(ids))
	var missing []string
	s.usersMu.Lock()
	for _, id := range ids {
		if !userIDRe.MatchString(id) {
			continue
		}
		if u, ok := s.users[id]; ok {
			out[id] = *u
		} else {
			missing = append(missing, id)
		}
	}
	s.usersMu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	loaded, err := s.store.LoadUsers(ctx, missing)
	if err != nil {
		log.Printf("load users: %v", err)
		return out, err
	}
	s.usersMu.Lock()
	for id, u := range loaded {
		if _, ok := s.users[id]; !ok { // a concurrent load may have won; keep its pending score
			u.Score += s.pendingScores[id]
			s.users[id] = &u
		}
		out[id] = *s.users[id]
	}
	s.usersMu.Unlock()
	return out, nil
}

func floorDiv(a, b int64) int64 {
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q
}

func (s *server) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()
	evictTicker := time.NewTicker(time.Minute)
	defer evictTicker.Stop()

	for {
		select {
		case <-ticker.C:
			s.flushAll(ctx) // a backlog drains in batches instead of waiting 2s per batch
			if s.scoreDirty.Swap(false) {
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				s.pushRanking(pctx, true)
				cancel()
			}
		case <-evictTicker.C:
			// Flush first so evicted chunks are already saved; same goroutine, so no flush is in flight.
			s.flushAll(ctx)
			s.rates.sweep()
			subscribed := s.hub.SubscribedKeys()
			n := s.world.Evict(func(cx, cy int64) bool {
				_, ok := subscribed[realtime.ChunkKey(cx, cy)]
				return ok
			})
			if n > 0 {
				log.Printf("evicted %d idle chunks", n)
			}
		case <-ctx.Done():
			return
		}
	}
}

// flushOnce saves pending scores and one batch of dirty chunks; false means the chunk save failed.
func (s *server) flushOnce(ctx context.Context) bool {
	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	s.usersMu.Lock()
	scores := s.pendingScores
	s.pendingScores = make(map[string]int64)
	s.usersMu.Unlock()
	if err := s.store.AddScores(flushCtx, scores); err != nil {
		log.Printf("score flush failed: %v", err)
		s.usersMu.Lock()
		for id, d := range scores {
			s.pendingScores[id] += d
		}
		s.usersMu.Unlock()
	}

	records := s.world.DirtySnapshots(maxFlushChunks)
	if len(records) == 0 {
		return true
	}

	if err := s.store.SaveChunks(flushCtx, s.cfg.WorldID, records); err != nil {
		log.Printf("store flush failed: %v", err)
		s.world.MarkDirty(records)
		return false
	}
	return true
}

// maxFlushChunks bounds one save: a transaction of this many chunks finishes well inside the
// timeout, and a failure only retries this batch instead of the whole backlog.
const maxFlushChunks = 50

// flushAll keeps saving batches until nothing is dirty, a save fails, or ctx ends.
func (s *server) flushAll(ctx context.Context) {
	for ctx.Err() == nil && s.world.HasDirty() {
		if !s.flushOnce(ctx) {
			return
		}
	}
	s.flushOnce(ctx) // scores, and anything dirtied meanwhile
}
