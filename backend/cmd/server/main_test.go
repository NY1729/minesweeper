package main

import (
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
