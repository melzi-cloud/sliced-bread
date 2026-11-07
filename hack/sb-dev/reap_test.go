package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLockFileExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reaper.lock")
	ctx := context.Background()
	unlock, err := lockFile(ctx, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(ctx, path, 0); !errors.Is(err, errLocked) {
		t.Fatalf("second lock: err = %v, want errLocked", err)
	}
	go func() { time.Sleep(500 * time.Millisecond); unlock() }()
	unlock2, err := lockFile(ctx, path, 5*time.Second)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}
