package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/config"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

// reap runs the reaper once. Runs never overlap: a run holds an exclusive lock
// on a per-user lock file, and a second run waits up to -wait for it, then
// skips. Every line it prints carries a timestamp, for the scheduled reaper's log.
func reap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	wait := fs.Duration("wait", 0, "how long to wait for another reaper run to finish (0: skip at once)")
	timeout := fs.Duration("timeout", 0, "give up after this long (0: no limit)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logf := timestamped(os.Stderr)

	lockPath, err := reaperLockPath()
	if err != nil {
		return err
	}
	unlock, err := lockFile(ctx, lockPath, *wait)
	if errors.Is(err, errLocked) {
		logf("[reaper] another reaper run holds %s; skipping", lockPath)
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()

	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	cfg, err := config.LoadDev()
	if err != nil {
		return err
	}
	p := tenant.New(cfg, "") // the reaper reads no run state
	p.Logf = logf
	runs, err := p.Reap(ctx, time.Now())
	logf("[reaper] reaped %d run(s) %v", len(runs), runs)
	return err
}

func timestamped(w io.Writer) func(string, ...any) {
	return func(f string, a ...any) {
		fmt.Fprintf(w, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(f, a...))
	}
}

// reaperLockPath is shared by every checkout of the repository and by the
// scheduled reaper, so manual and scheduled runs exclude each other.
func reaperLockPath() (string, error) {
	if p := os.Getenv("SB_REAPER_LOCK"); p != "" {
		return p, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "slicedbread-reaper.lock"), nil
}

var errLocked = errors.New("locked")

// lockFile takes an exclusive flock on path, polling for up to wait. The lock is
// released by the returned function or when the process exits, so a killed run
// never leaves a stale lock behind.
func lockFile(ctx context.Context, path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errLocked
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
