package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveStderr collects what is written to os.Stderr while it is redirected, so a test can
// wait for a log line produced by a background goroutine.
type liveStderr struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *liveStderr) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *liveStderr) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// redirectStderr points os.Stderr at a pipe until restore is called (also run on cleanup).
func redirectStderr(t *testing.T) (*liveStderr, func()) {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = wr
	ls := &liveStderr{}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(ls, rd)
		close(copied)
	}()
	var once sync.Once
	restore := func() {
		once.Do(func() {
			os.Stderr = orig
			wr.Close()
			<-copied
			rd.Close()
		})
	}
	t.Cleanup(restore)
	return ls, restore
}

// waitForText blocks until ls contains want, failing the test after a generous deadline.
func waitForText(t *testing.T, ls *liveStderr, want string) {
	t.Helper()
	waitFor(t, "stderr to contain "+want, func() bool { return strings.Contains(ls.String(), want) })
}

// settleTimeout bounds waits on channels in tests (never a pacing sleep).
const settleTimeout = 10 * time.Second
