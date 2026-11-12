package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// exitPeerRunning is EX_TEMPFAIL: another bridge holds the store lock (running or starting).
const exitPeerRunning = 75

// errLockHeld means another process holds the single-instance lock.
var errLockHeld = errors.New("another bridge already holds the store lock")

// instanceLock is an advisory lock on <store>/bridge.lock (one bridge per store dir).
// It does not protect copies of the same store: one bridge per WhatsApp login.
type instanceLock struct {
	f *os.File
}

// acquireLock takes the lock at path, returning errLockHeld when a peer owns it.
func acquireLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: path derived from the store dir
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := tryLockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &instanceLock{f: f}, nil
}

// Release drops the lock; the file itself stays (flock is tied to the descriptor).
func (l *instanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
}

// loadToken returns the API token: WHATSAPP_BRIDGE_TOKEN wins over <store>/bridge.token.
// An absent token file yields "" (auth disabled). The token is never logged.
func loadToken(dir string) (string, error) {
	if t := strings.TrimSpace(os.Getenv("WHATSAPP_BRIDGE_TOKEN")); t != "" {
		return t, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "bridge.token")) //nolint:gosec // G304: fixed name inside the store dir
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		// Fail closed: an existing but empty token file must not silently disable auth.
		return "", errors.New("token file bridge.token exists but is empty; delete it or write a token")
	}
	return t, nil
}

// idleTracker records the last API activity for the optional idle exit.
type idleTracker struct {
	last atomic.Int64 // unix nanoseconds
}

func newIdleTracker() *idleTracker {
	t := &idleTracker{}
	t.Touch()
	return t
}

// Touch marks activity now.
func (t *idleTracker) Touch() { t.last.Store(time.Now().UnixNano()) }

// idleFor returns the time since the last activity.
func (t *idleTracker) idleFor() time.Duration {
	return time.Since(time.Unix(0, t.last.Load()))
}

// idleExitDuration parses WHATSAPP_BRIDGE_IDLE_EXIT; 0 means disabled.
func idleExitDuration() time.Duration {
	v := os.Getenv("WHATSAPP_BRIDGE_IDLE_EXIT")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		logf("Ignoring invalid WHATSAPP_BRIDGE_IDLE_EXIT %q\n", v)
		return 0
	}
	return d
}

// watchIdle calls onIdle once no activity has been seen for d (d <= 0 disables).
func watchIdle(ctx context.Context, t *idleTracker, d time.Duration, onIdle func()) {
	if d <= 0 {
		return
	}
	tick := d / 4
	if tick > time.Second {
		tick = time.Second
	}
	if tick < time.Millisecond {
		tick = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if t.idleFor() >= d {
					onIdle()
					return
				}
			}
		}
	}()
}
