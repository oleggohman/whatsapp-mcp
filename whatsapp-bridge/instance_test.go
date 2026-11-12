package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireLockOpenError(t *testing.T) {
	_, err := acquireLock(filepath.Join(t.TempDir(), "missing-dir", "bridge.lock"))
	if err == nil || !strings.Contains(err.Error(), "open lock file") {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, errLockHeld) {
		t.Fatal("an open failure must not look like a held lock")
	}
}

func TestAcquireLockHeldAndReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.lock")
	l1, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(path); !errors.Is(err, errLockHeld) {
		t.Fatalf("second lock err = %v, want errLockHeld", err)
	}
	l1.Release()
	l1.Release() // idempotent
	var nilLock *instanceLock
	nilLock.Release()
	l2, err := acquireLock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	l2.Release()
}

func TestLoadToken(t *testing.T) {
	t.Run("env wins over file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bridge.token"), []byte("file-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("WHATSAPP_BRIDGE_TOKEN", "  env-token ")
		if got, err := loadToken(dir); err != nil || got != "env-token" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("file is trimmed", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
		if err := os.WriteFile(filepath.Join(dir, "bridge.token"), []byte("file-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := loadToken(dir); err != nil || got != "file-token" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("absent file disables auth", func(t *testing.T) {
		t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
		if got, err := loadToken(t.TempDir()); err != nil || got != "" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("empty file fails closed", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
		if err := os.WriteFile(filepath.Join(dir, "bridge.token"), []byte(" \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadToken(dir); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unreadable file is an error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
		// A directory in place of the file cannot be read
		if err := os.Mkdir(filepath.Join(dir, "bridge.token"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := loadToken(dir); err == nil || !strings.Contains(err.Error(), "read token file") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestIdleExitDuration(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
		warn bool
	}{
		{"", 0, false},
		{"90s", 90 * time.Second, false},
		{"bogus", 0, true},
		{"-5s", 0, true},
	}
	for _, tc := range cases {
		t.Setenv("WHATSAPP_BRIDGE_IDLE_EXIT", tc.env)
		var got time.Duration
		out := captureStderr(t, func() { got = idleExitDuration() })
		if got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.env, got, tc.want)
		}
		if warned := strings.Contains(out, "Ignoring invalid"); warned != tc.warn {
			t.Errorf("%q: warned=%v, want %v (%q)", tc.env, warned, tc.warn, out)
		}
	}
}

func TestWatchIdleFiresOnceWhenIdle(t *testing.T) {
	// d/4 is below the 1 ms floor, so the ticker uses the floor
	tr := newIdleTracker()
	tr.last.Store(time.Now().Add(-time.Hour).UnixNano())
	fired := make(chan struct{}, 2)
	watchIdle(context.Background(), tr, 2*time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("onIdle was not called")
	}
}

func TestWatchIdleDisabledAndCancelled(t *testing.T) {
	tr := newIdleTracker()
	called := make(chan struct{}, 1)
	watchIdle(context.Background(), tr, 0, func() { called <- struct{}{} })
	// A long window caps the tick at one second; cancelling stops the watcher before it fires
	ctx, cancel := context.WithCancel(context.Background())
	watchIdle(ctx, tr, time.Hour, func() { called <- struct{}{} })
	cancel()
	select {
	case <-called:
		t.Fatal("onIdle must not be called")
	default:
	}
}
