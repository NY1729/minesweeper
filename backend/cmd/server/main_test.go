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
	if !ok(strings.Repeat("a", 256)) || !ok(strings.Repeat("v", 256)) || !ok(strings.Repeat("0", 256)) {
		t.Fatal("palette indices 0-31 must be accepted")
	}
	if ok(strings.Repeat("w", 256)) || ok(strings.Repeat("A", 256)) || ok(strings.Repeat("a", 255)) || ok(strings.Repeat("a", 257)) {
		t.Fatal("out of range indices and wrong lengths must be rejected")
	}
}
