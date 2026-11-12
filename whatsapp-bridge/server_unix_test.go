//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readMediaFile must check the file it reads, not a path that may have changed since
// validation: FIFOs (which would block a plain open) and oversized files are refused.
func TestReadMediaFileChecksOpenedFile(t *testing.T) {
	dir := t.TempDir()
	old := maxMediaBytes
	maxMediaBytes = 1024
	t.Cleanup(func() { maxMediaBytes = old })

	ok := filepath.Join(dir, "ok.bin")
	if err := os.WriteFile(ok, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := readMediaFile(ok); err != nil || string(b) != "data" {
		t.Fatalf("regular file: %q %v", b, err)
	}

	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMediaFile(big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("big file: %v", err)
	}
	if _, err := readMediaFile("rel.bin"); err == nil {
		t.Fatal("relative path accepted")
	}

	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := readMediaFile(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readMediaFile blocked opening a FIFO")
	}

	// The send path uses the same checks: a FIFO is refused without uploading.
	fc := &fakeClient{connected: true}
	if okSend, msg := sendWhatsAppMessage(fc, "1", "", fifo); okSend || fc.uploads != 0 {
		t.Fatalf("send with fifo: ok=%v msg=%q uploads=%d", okSend, msg, fc.uploads)
	}
}
