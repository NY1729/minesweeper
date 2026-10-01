package main

import (
	"strings"
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	b := newBucket(10, 20)
	now := time.Now()
	n := 0
	for i := 0; i < 50; i++ {
		if b.allow(now) {
			n++
		}
	}
	if n != 20 {
		t.Fatalf("burst allowed %d, want 20", n)
	}
	if !b.allow(now.Add(100 * time.Millisecond)) {
		t.Fatal("should refill 1 token after 100ms at 10/s")
	}
	if b.allow(now.Add(100 * time.Millisecond)) {
		t.Fatal("only one token should have refilled")
	}
}

func TestRateLimitsSurviveReconnect(t *testing.T) {
	var r rateLimits
	n := 0
	for i := 0; i < 100; i++ { // 100 "connections", same user
		if r.allow("act:alice", 6, 12) {
			n++
		}
	}
	if n != 12 {
		t.Fatalf("allowed %d across connections, want burst 12", n)
	}
	if !r.allow("act:bob", 6, 12) {
		t.Fatal("other users are unaffected")
	}
}

func TestPixelsRe(t *testing.T) {
	ok := func(s string) bool { return pixelsRe.MatchString(s) }
	rgba := func(px string) string { return strings.Repeat(px, 256) }

	// current format: alpha 0 or f, then r g b
	for _, px := range []string{"0000", "f000", "ffff", "fd45", "f0a9"} {
		if !ok(rgba(px)) {
			t.Fatalf("pixel %q must be accepted", px)
		}
	}
	// legacy format: palette indices 0-31
	for _, c := range []string{"0", "a", "v"} {
		if !ok(strings.Repeat(c, 256)) {
			t.Fatalf("legacy digit %q must be accepted", c)
		}
	}
	bad := map[string]string{
		"semi-transparent": rgba("8123"),
		"uppercase":        rgba("fABC"),
		"non-hex channel":  rgba("f12g"),
		"legacy too high":  strings.Repeat("w", 256),
		"legacy short":     strings.Repeat("a", 255),
		"legacy long":      strings.Repeat("a", 257),
		"rgba short":       rgba("f123")[:1020],
		"rgba long":        rgba("f123") + "f123",
		"mixed lengths":    strings.Repeat("a", 512),
		"empty":            "",
	}
	for name, s := range bad {
		if ok(s) {
			t.Fatalf("%s must be rejected", name)
		}
	}
}
