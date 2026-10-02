// Load generator: N simulated players over WebSocket, each behaving like a person
// (looks at a 7x4 chunk area, opens/flags cells, moves its selected cell, saves a profile).
//
//	spread:  every player looks at its own area (little overlap)
//	crowded: every player looks at the same area (worst case for broadcasts)
//
// Run it next to the backend so the load does not go through Cloudflare (see scripts/loadtest.sh).
// Against the production database it writes users named "load-N" and chunks near -chunk-base;
// remove them afterwards with -cleanup (scripts/loadtest-cleanup.sh).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	url       = flag.String("url", "ws://localhost:18080/api/ws", "websocket url")
	health    = flag.String("health", "http://localhost:18080/api/health", "health url")
	origin    = flag.String("origin", "https://mine.alt0.org", "Origin header")
	n         = flag.Int("n", 100, "players")
	dur       = flag.Duration("dur", 30*time.Second, "steady-state duration after ramp")
	ramp      = flag.Int("ramp", 200, "new connections per second")
	mode      = flag.String("mode", "spread", "spread | crowded")
	cursor    = flag.Duration("cursor", 200*time.Millisecond, "cursor update interval (0 = off)")
	actions   = flag.Duration("action", 800*time.Millisecond, "mean time between reveal/flag actions")
	flagsOnly = flag.Bool("flags-only", false, "only toggle flags (no reveals, so no score changes)")
	cleanup   = flag.Bool("cleanup", false, "delete the data a previous run left in Turso (users load-0..-cleanup-max, chunks near -chunk-base) and exit; needs TURSO_DATABASE_URL and TURSO_AUTH_TOKEN")
	cleanMax  = flag.Int("cleanup-max", 10000, "with -cleanup: highest player count any earlier run used")
	spoofIP   = flag.Bool("spoof-ip", false, "send a distinct CF-Connecting-IP per player (only for a backend reached directly; Cloudflare rejects requests carrying that header)")
	chunkBase = flag.Int64("chunk-base", 0, "shift every player's area by this many chunks in x and y (to test far away from real players)")
)

type stats struct {
	connected, failed, dropped atomic.Int64
	msgs, bytes                atomic.Int64
	cursorUpdates              atomic.Int64 // cursor positions delivered, whether one per message or batched
	sent                       atomic.Int64
	mu                         sync.Mutex
	byType                     map[string]int64
	lat, flagLat               []time.Duration
	healthMax                  time.Duration
	healthFail                 int
}

var st = &stats{byType: map[string]int64{}}

var types = [][]byte{
	[]byte(`"type":"chunk"`), []byte(`"type":"reveal"`), []byte(`"type":"flag"`),
	[]byte(`"type":"cursor"`), []byte(`"type":"cursors"`), []byte(`"type":"users"`),
	[]byte(`"type":"ranking"`), []byte(`"type":"score"`), []byte(`"type":"me"`), []byte(`"type":"hello"`),
	[]byte(`"type":"cursorbatch"`),
}

func classify(m []byte) string {
	// "cursor" is a prefix of "cursors": check the longer one first
	if bytes.Contains(m, types[4]) {
		return "cursors"
	}
	for _, t := range types {
		if bytes.Contains(m, t) {
			return string(t[8 : len(t)-1])
		}
	}
	return "other"
}

type flagMsg struct {
	X, Y int64
}

type revealMsg struct {
	Type  string `json:"type"`
	Cells []struct {
		X, Y int64
	} `json:"cells"`
}

func player(i int, stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	hdr := http.Header{}
	hdr.Set("Origin", *origin)
	if *spoofIP {
		hdr.Set("CF-Connecting-IP", fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)) // distinct "IP" per player
	}
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second, ReadBufferSize: 16 << 10, WriteBufferSize: 4 << 10}
	conn, resp, err := d.Dial(*url, hdr)
	if err != nil {
		if st.failed.Add(1) == 1 { // show the first failure only
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			fmt.Fprintf(os.Stderr, "connect failed: %v (HTTP %d)\n", err, code)
		}
		return
	}
	st.connected.Add(1)
	defer conn.Close()

	rng := rand.New(rand.NewSource(int64(i) + 1))
	var cx, cy int64
	if *mode == "crowded" {
		cx, cy = int64(rng.Intn(3))-1, int64(rng.Intn(2))-1
	} else {
		cx, cy = int64(rng.Intn(400)-200)*8, int64(rng.Intn(400)-200)*5
	}
	cx, cy = cx+*chunkBase, cy+*chunkBase
	const w, h = 7, 4 // chunks in view
	x0, y0 := cx*32, cy*32
	chunks := make([]map[string]int64, 0, w*h)
	for dy := int64(0); dy < h; dy++ {
		for dx := int64(0); dx < w; dx++ {
			chunks = append(chunks, map[string]int64{"x": cx + dx, "y": cy + dy})
		}
	}

	var wmu sync.Mutex
	send := func(v any) bool {
		wmu.Lock()
		defer wmu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if conn.WriteJSON(v) != nil {
			return false
		}
		st.sent.Add(1)
		return true
	}

	var pmu sync.Mutex
	pending := map[[2]int64]time.Time{}

	done := make(chan struct{})
	go func() { // reader
		defer close(done)
		for {
			_, m, err := conn.ReadMessage()
			if err != nil {
				return
			}
			st.msgs.Add(1)
			st.bytes.Add(int64(len(m)))
			t := classify(m)
			switch t {
			case "cursor":
				st.cursorUpdates.Add(1)
			case "cursorbatch":
				st.cursorUpdates.Add(int64(bytes.Count(m, []byte(`"s":`))))
			}
			st.mu.Lock()
			st.byType[t]++
			st.mu.Unlock()
			if t == "flag" {
				var fm flagMsg
				if json.Unmarshal(m, &fm) == nil {
					pmu.Lock()
					if t0, ok := pending[[2]int64{fm.X, fm.Y}]; ok {
						delete(pending, [2]int64{fm.X, fm.Y})
						st.mu.Lock()
						st.flagLat = append(st.flagLat, time.Since(t0))
						st.mu.Unlock()
					}
					pmu.Unlock()
				}
			}
			if t == "reveal" {
				var rm revealMsg
				if json.Unmarshal(m, &rm) == nil && len(rm.Cells) > 0 {
					now := time.Now()
					pmu.Lock()
					for _, c := range rm.Cells {
						k := [2]int64{c.X, c.Y}
						if t0, ok := pending[k]; ok {
							delete(pending, k)
							st.mu.Lock()
							st.lat = append(st.lat, now.Sub(t0))
							st.mu.Unlock()
						}
					}
					pmu.Unlock()
				}
			}
		}
	}()

	px := make([]byte, 0, 1024)
	for k := 0; k < 256; k++ {
		px = append(px, fmt.Sprintf("f%03x", rng.Intn(4096))...)
	}
	send(map[string]any{"type": "auth", "secret": secretFor(i)})
	send(map[string]any{"type": "subscribe", "chunks": chunks})
	send(map[string]any{"type": "setProfile", "pixels": string(px), "name": fmt.Sprintf("load-%d", i)})

	randCell := func() (int64, int64) { return x0 + int64(rng.Intn(w*32)), y0 + int64(rng.Intn(h*32)) }
	var curC <-chan time.Time
	if *cursor > 0 {
		tk := time.NewTicker(*cursor)
		defer tk.Stop()
		curC = tk.C
	}
	next := time.After(time.Duration(rng.Int63n(int64(*actions))))
	for {
		select {
		case <-stop:
			return
		case <-done:
			st.dropped.Add(1)
			return
		case <-curC:
			x, y := randCell()
			if !send(map[string]any{"type": "cursor", "px": x, "py": y}) {
				st.dropped.Add(1)
				return
			}
		case <-next:
			x, y := randCell()
			if *flagsOnly || rng.Intn(10) < 3 {
				pmu.Lock()
				pending[[2]int64{x, y}] = time.Now()
				pmu.Unlock()
				send(map[string]any{"type": "flag", "x": x, "y": y})
			} else {
				pmu.Lock()
				pending[[2]int64{x, y}] = time.Now()
				if len(pending) > 200 {
					for k := range pending { // forget unanswered ones (cell already open => no broadcast)
						delete(pending, k)
						break
					}
				}
				pmu.Unlock()
				send(map[string]any{"type": "reveal", "x": x, "y": y})
			}
			next = time.After(time.Duration(float64(*actions) * (0.5 + rng.Float64())))
		}
	}
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	return d[int(float64(len(d)-1)*p)]
}

func main() {
	flag.Parse()
	if *cleanup {
		cleanupTurso()
		return
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup

	go func() { // health probe: does the server still answer ordinary requests?
		c := http.Client{Timeout: 5 * time.Second}
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			t0 := time.Now()
			r, err := c.Get(*health)
			el := time.Since(t0)
			st.mu.Lock()
			if err != nil || r.StatusCode != 200 {
				st.healthFail++
			}
			if el > st.healthMax {
				st.healthMax = el
			}
			st.mu.Unlock()
			if r != nil {
				r.Body.Close()
			}
		}
	}()

	start := time.Now()
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go player(i, stop, &wg)
		if (i+1)%*ramp == 0 {
			time.Sleep(time.Second)
		}
	}
	fmt.Fprintf(os.Stderr, "ramp done in %.1fs: %d connected, %d failed\n", time.Since(start).Seconds(), st.connected.Load(), st.failed.Load())

	// steady state, with a progress line every 5 seconds
	base := st.msgs.Load()
	baseT := time.Now()
	end := time.Now().Add(*dur)
	for time.Now().Before(end) {
		time.Sleep(5 * time.Second)
		now := time.Now()
		cur := st.msgs.Load()
		fmt.Fprintf(os.Stderr, "  t+%2.0fs  conns=%d dropped=%d  recv %.0f msg/s\n", now.Sub(start).Seconds(), st.connected.Load()-st.dropped.Load(), st.dropped.Load(), float64(cur-base)/now.Sub(baseT).Seconds())
		base, baseT = cur, now
	}
	close(stop)
	wg.Wait()

	st.mu.Lock()
	defer st.mu.Unlock()
	sort.Slice(st.lat, func(a, b int) bool { return st.lat[a] < st.lat[b] })
	total := time.Since(start).Seconds()
	fmt.Printf("mode=%s players=%d connected=%d failed=%d dropped_by_server=%d\n", *mode, *n, st.connected.Load(), st.failed.Load(), st.dropped.Load())
	fmt.Printf("sent %d msgs (%.0f/s)  recv %d msgs (%.0f/s)  %.1f MB (%.1f MB/s)\n", st.sent.Load(), float64(st.sent.Load())/total, st.msgs.Load(), float64(st.msgs.Load())/total, float64(st.bytes.Load())/1e6, float64(st.bytes.Load())/1e6/total)
	var parts []string
	for _, t := range []string{"chunk", "reveal", "flag", "cursor", "cursorbatch", "cursors", "users", "ranking", "score", "me"} {
		parts = append(parts, fmt.Sprintf("%s=%d", t, st.byType[t]))
	}
	fmt.Println("received by type:", strings.Join(parts, " "))
	fmt.Printf("cursor positions delivered: %d (%.0f/s)\n", st.cursorUpdates.Load(), float64(st.cursorUpdates.Load())/total)
	fmt.Printf("reveal round trip (n=%d): p50=%v p95=%v p99=%v max=%v\n", len(st.lat), pct(st.lat, .5).Round(time.Millisecond), pct(st.lat, .95).Round(time.Millisecond), pct(st.lat, .99).Round(time.Millisecond), pct(st.lat, 1).Round(time.Millisecond))
	sort.Slice(st.flagLat, func(a, b int) bool { return st.flagLat[a] < st.flagLat[b] })
	fmt.Printf("flag round trip   (n=%d): p50=%v p95=%v p99=%v max=%v\n", len(st.flagLat), pct(st.flagLat, .5).Round(time.Millisecond), pct(st.flagLat, .95).Round(time.Millisecond), pct(st.flagLat, .99).Round(time.Millisecond), pct(st.flagLat, 1).Round(time.Millisecond))
	fmt.Printf("health endpoint: slowest=%v failures=%d\n", st.healthMax.Round(time.Millisecond), st.healthFail)
}

func secretFor(i int) string { return fmt.Sprintf("load-%06d-0123456789abcdef", i) }

// userID mirrors the server: first 8 bytes of sha256(secret), hex.
func userID(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}

// cleanupTurso removes exactly what this tool creates: the test users (by id, derived from
// the same secrets) and chunks far from the real world (-chunk-base must be large, so a
// wrong flag can never touch the area real players use).
func cleanupTurso() {
	url, token := os.Getenv("TURSO_DATABASE_URL"), os.Getenv("TURSO_AUTH_TOKEN")
	if url == "" {
		fmt.Fprintln(os.Stderr, "TURSO_DATABASE_URL is not set")
		os.Exit(1)
	}
	if *chunkBase < 100000 {
		fmt.Fprintln(os.Stderr, "refusing to delete chunks: -chunk-base must be >= 100000 (test data lives far from the real world)")
		os.Exit(1)
	}
	endpoint := strings.TrimRight(strings.Replace(url, "libsql://", "https://", 1), "/") + "/v2/pipeline"

	type arg struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	type stmt struct {
		SQL  string `json:"sql"`
		Args []arg  `json:"args,omitempty"`
	}
	type request struct {
		Type string `json:"type"`
		Stmt *stmt  `json:"stmt"`
	}
	run := func(sql string, args ...arg) int64 {
		body, _ := json.Marshal(map[string]any{"requests": []request{{Type: "execute", Stmt: &stmt{SQL: sql, Args: args}}}})
		req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "turso:", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out struct {
			Results []struct {
				Type     string `json:"type"`
				Response struct {
					Result struct {
						Affected int64 `json:"affected_row_count"`
					} `json:"result"`
				} `json:"response"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"results"`
		}
		if resp.StatusCode/100 != 2 || json.Unmarshal(data, &out) != nil || len(out.Results) == 0 || out.Results[0].Type != "ok" {
			fmt.Fprintf(os.Stderr, "turso: HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(data)))
			os.Exit(1)
		}
		return out.Results[0].Response.Result.Affected
	}

	var users int64
	for from := 0; from < *cleanMax; from += 500 {
		to := min(from+500, *cleanMax)
		var args []arg
		marks := make([]string, 0, to-from)
		for i := from; i < to; i++ {
			args = append(args, arg{"text", userID(secretFor(i))})
			marks = append(marks, "?")
		}
		users += run("DELETE FROM users WHERE id IN ("+strings.Join(marks, ",")+")", args...)
	}
	lo := fmt.Sprint(*chunkBase - 2000) // players are placed within about +-1700 chunks of the base
	chunks := run("DELETE FROM chunks WHERE chunk_x >= ? AND chunk_y >= ?", arg{"integer", lo}, arg{"integer", lo})
	fmt.Printf("deleted %d test users and %d test chunks\n", users, chunks)
}
