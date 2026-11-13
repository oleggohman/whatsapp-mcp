//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// syscall.Kill does not exist on Windows, so this test is POSIX-only.
func TestDefaultSignalHookDeliversSIGTERM(t *testing.T) {
	h := newRunHarness(t, nil)
	c := make(chan os.Signal, 1)
	h.realSignals(c)
	defer signal.Stop(c)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-c:
		if got != syscall.SIGTERM {
			t.Fatalf("signal = %v", got)
		}
	case <-time.After(settleTimeout):
		t.Fatal("SIGTERM not delivered to the registered channel")
	}
}
