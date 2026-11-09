package patterns

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadBatchKeepsOrder(t *testing.T) {
	keys := []string{"c", "a", "b"}
	got, err := LoadBatch(context.Background(), keys, 2, func(_ context.Context, key string) (string, error) {
		return key + "-value", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c-value", "a-value", "b-value"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestLoadBatchReturnsFirstInputError(t *testing.T) {
	firstErr := errors.New("first")
	secondErr := errors.New("second")
	_, err := LoadBatch(context.Background(), []string{"a", "b"}, 2, func(_ context.Context, key string) (string, error) {
		if key == "a" {
			return "", firstErr
		}
		return "", secondErr
	})
	if !errors.Is(err, firstErr) {
		t.Fatalf("got %v want %v", err, firstErr)
	}
}

func TestDrainPagesCanBeCancelledDuringDelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := DrainPages(ctx, time.Second, func(context.Context) (PageResult, error) {
		return PageResult{Rows: 1}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v want deadline exceeded", err)
	}
}

func TestSnapshotStore(t *testing.T) {
	store := NewSnapshotStore(&Snapshot{Version: 1, Entries: map[string]string{"a": "1"}})
	store.Store(&Snapshot{Version: 2, Entries: map[string]string{"a": "2"}})
	if got := store.Load(); got == nil || got.Version != 2 || got.Entries["a"] != "2" {
		t.Fatalf("unexpected snapshot: %#v", got)
	}
}

func TestSafeGoRecoversPanic(t *testing.T) {
	var called int32
	done := make(chan struct{})
	SafeGo(func(any, []byte) {
		atomic.StoreInt32(&called, 1)
		close(done)
	}, func() { panic("boom") })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panic handler was not called")
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatal("handler not called")
	}
}
