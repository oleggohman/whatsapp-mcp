package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// failWriter is a ResponseWriter whose body writes fail.
type failWriter struct{ header http.Header }

func (w *failWriter) Header() http.Header       { return w.header }
func (w *failWriter) WriteHeader(int)           {}
func (w *failWriter) Write([]byte) (int, error) { return 0, errors.New("client went away") }

func TestWriteJSONLogsEncodeFailure(t *testing.T) {
	out := captureStderr(t, func() {
		writeJSON(&failWriter{header: http.Header{}}, http.StatusOK, map[string]string{"a": "b"})
	})
	if !strings.Contains(out, "Failed to write response: client went away") {
		t.Fatalf("log = %q", out)
	}
}

func TestWriteJSONSetsHeadersAndBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusTeapot, map[string]int{"n": 1})
	if rec.Code != http.StatusTeapot || rec.Header().Get("Content-Type") != "application/json" || strings.TrimSpace(rec.Body.String()) != `{"n":1}` {
		t.Fatalf("code=%d ct=%q body=%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestNewAPIServerStoreDirFallbackWhenAbsFails(t *testing.T) {
	t.Setenv("WHATSAPP_STORE_DIR", "rel-store")
	old := filepathAbs
	filepathAbs = func(string) (string, error) { return "", errors.New("no cwd") }
	t.Cleanup(func() { filepathAbs = old })
	a := newAPIServer(func() bridgeClient { return nil }, nil, "", "127.0.0.1:8080")
	if a.storeDirAbs != "rel-store" {
		t.Fatalf("storeDirAbs = %q", a.storeDirAbs)
	}
}

func TestNewAPIServerHostAllowlist(t *testing.T) {
	a := newAPIServer(func() bridgeClient { return nil }, nil, "", "Bridge.Example:9000")
	if !a.hostAllowed("bridge.example:9000") {
		t.Fatal("configured host must be allowed")
	}
	if u := newAPIServer(func() bridgeClient { return nil }, nil, "", "0.0.0.0:9000"); len(u.extraHosts) != 0 {
		t.Fatalf("unspecified address must not be allowlisted: %v", u.extraHosts)
	}
}

func TestDownloadHandlerReportsFailure(t *testing.T) {
	fc := &fakeClient{connected: true}
	_, h := newTestAPI(t, fc, "")
	rec := do(h, http.MethodPost, "/api/download", `{"message_id":"nope","chat_jid":"1@s.whatsapp.net"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d", rec.Code)
	}
	resp := decode[DownloadMediaResponse](t, rec.Body.String())
	if resp.Success || !strings.HasPrefix(resp.Message, "Failed to download media: failed to find message") {
		t.Fatalf("resp = %+v", resp)
	}
}

// acceptFailListener fails Accept so that Serve returns a non-shutdown error.
type acceptFailListener struct {
	net.Listener
	failed chan struct{}
}

func (l *acceptFailListener) Accept() (net.Conn, error) {
	defer close(l.failed)
	return nil, errors.New("accept boom")
}

func TestStartRESTServerLogsServeError(t *testing.T) {
	old := listenTCP
	t.Cleanup(func() { listenTCP = old })
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	failed := make(chan struct{})
	listenTCP = func(string) (net.Listener, error) { return &acceptFailListener{Listener: real, failed: failed}, nil }

	logs, restore := redirectStderr(t)
	if _, _, err = startRESTServer("127.0.0.1:0", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	<-failed
	waitForText(t, logs, "REST API server error: accept boom")
	restore()
}

func TestStartRESTServerListenError(t *testing.T) {
	_, _, err := startRESTServer("not-an-address", http.NotFoundHandler())
	if err == nil || !strings.Contains(err.Error(), "failed to listen on not-an-address") {
		t.Fatalf("err = %v", err)
	}
}
