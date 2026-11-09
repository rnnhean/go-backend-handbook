package ratelimit

import (
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	l := New(2, time.Second)
	if !l.Allow("u-1") || !l.Allow("u-1") {
		t.Fatal("first two requests should pass")
	}
	if l.Allow("u-1") {
		t.Fatal("third request should be rejected")
	}
	if !l.Allow("u-2") {
		t.Fatal("different key must not share the limit")
	}
}
