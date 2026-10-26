package lock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "lock", "tunlr.lock")

	first, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire: err = %v, want ErrBusy", err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Acquire(ctx, filepath.Join(t.TempDir(), "tunlr.lock"))
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want context.Canceled and not ErrBusy", err)
	}
}
