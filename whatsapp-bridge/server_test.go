package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// fakeClient implements bridgeClient without touching the network.
type fakeClient struct {
	connected bool
	uploads   int
	sends     []types.JID
	downloads int
	data      []byte
}

func (f *fakeClient) IsConnected() bool { return f.connected }

func (f *fakeClient) Upload(_ context.Context, b []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploads++
	return whatsmeow.UploadResponse{URL: "u", DirectPath: "/d", FileLength: uint64(len(b))}, nil
}

func (f *fakeClient) SendMessage(_ context.Context, to types.JID, _ *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sends = append(f.sends, to)
	return whatsmeow.SendResponse{}, nil
}

func (f *fakeClient) Download(_ context.Context, _ whatsmeow.DownloadableMessage) ([]byte, error) {
	f.downloads++
	return f.data, nil
}

func newTestAPI(t *testing.T, fc *fakeClient, token string) (*apiServer, http.Handler) {
	t.Helper()
	store := newTestStore(t)
	a := newAPIServer(func() bridgeClient { return fc }, store, token, "127.0.0.1:8080")
	return a, a.handler()
}

// do runs a request against h; opts mutate the request before it is served.
func do(h http.Handler, method, path, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSendValidation(t *testing.T) {
	fc := &fakeClient{connected: true}
	_, h := newTestAPI(t, fc, "")

	tests := []struct {
		name   string
		method string
		body   string
		want   int
	}{
		{"wrong method", http.MethodGet, "", http.StatusMethodNotAllowed},
		{"bad json", http.MethodPost, "{nope", http.StatusBadRequest},
		{"missing recipient", http.MethodPost, `{"message":"hi"}`, http.StatusBadRequest},
		{"missing message and media", http.MethodPost, `{"recipient":"123"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := do(h, tt.method, "/api/send", tt.body); rec.Code != tt.want {
				t.Fatalf("code = %d want %d (%s)", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
	if len(fc.sends) != 0 {
		t.Fatalf("validation failures must not send")
	}
}

func TestSendHappyPath(t *testing.T) {
	fc := &fakeClient{connected: true}
	_, h := newTestAPI(t, fc, "")
	rec := do(h, http.MethodPost, "/api/send", `{"recipient":"491701234567","message":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp SendMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success {
		t.Fatalf("resp = %+v err=%v", resp, err)
	}
	if len(fc.sends) != 1 || fc.sends[0].User != "491701234567" || fc.sends[0].Server != types.DefaultUserServer {
		t.Fatalf("sends = %v", fc.sends)
	}
}

func TestSendNotConnected(t *testing.T) {
	fc := &fakeClient{connected: false}
	_, h := newTestAPI(t, fc, "")
	rec := do(h, http.MethodPost, "/api/send", `{"recipient":"1","message":"hi"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Not connected") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSendNilClient(t *testing.T) {
	store := newTestStore(t)
	a := newAPIServer(func() bridgeClient { return nil }, store, "", "127.0.0.1:8080")
	rec := do(a.handler(), http.MethodPost, "/api/send", `{"recipient":"1","message":"hi"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Not connected") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSendMediaPathValidation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(file, []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	old := maxMediaBytes
	maxMediaBytes = 1024
	t.Cleanup(func() { maxMediaBytes = old })

	fc := &fakeClient{connected: true}
	_, h := newTestAPI(t, fc, "")

	post := func(path string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(SendMessageRequest{Recipient: "1", MediaPath: path})
		return do(h, http.MethodPost, "/api/send", string(b))
	}
	for name, p := range map[string]string{
		"relative": "pic.png",
		"dotdot":   "../pic.png",
		"dir":      dir,
		"missing":  filepath.Join(dir, "nope.png"),
		"too big":  big,
	} {
		if rec := post(p); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	if fc.uploads != 0 {
		t.Fatalf("rejected paths must not upload")
	}
	if rec := post(file); rec.Code != http.StatusOK {
		t.Fatalf("valid media: code = %d (%s)", rec.Code, rec.Body.String())
	}
	if fc.uploads != 1 || len(fc.sends) != 1 {
		t.Fatalf("uploads=%d sends=%d", fc.uploads, len(fc.sends))
	}
}

func TestGuard(t *testing.T) {
	fc := &fakeClient{connected: true}
	_, h := newTestAPI(t, fc, "")
	const body = `{"recipient":"1","message":"hi"}`

	tests := []struct {
		name string
		opts []func(*http.Request)
		want int
	}{
		{"ok", nil, http.StatusOK},
		{"text/plain", []func(*http.Request){func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }}, http.StatusUnsupportedMediaType},
		{"no content type", []func(*http.Request){func(r *http.Request) { r.Header.Del("Content-Type") }}, http.StatusUnsupportedMediaType},
		{"json with charset", []func(*http.Request){func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }}, http.StatusOK},
		{"origin present", []func(*http.Request){func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }}, http.StatusForbidden},
		{"foreign host", []func(*http.Request){func(r *http.Request) { r.Host = "evil.example:8080" }}, http.StatusForbidden},
		{"localhost host", []func(*http.Request){func(r *http.Request) { r.Host = "localhost:8080" }}, http.StatusOK},
		{"ipv6 host", []func(*http.Request){func(r *http.Request) { r.Host = "[::1]:8080" }}, http.StatusOK},
		{"host without port", []func(*http.Request){func(r *http.Request) { r.Host = "127.0.0.1" }}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := do(h, http.MethodPost, "/api/send", body, tt.opts...); rec.Code != tt.want {
				t.Fatalf("code = %d want %d (%s)", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestGuardOnlyChecksContentTypeOnPost(t *testing.T) {
	_, h := newTestAPI(t, &fakeClient{}, "")
	// GET without Content-Type reaches the handler (405 from the method check, not 415 from the guard)
	if rec := do(h, http.MethodGet, "/api/send", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d want 405", rec.Code)
	}
}

func TestConfiguredHostAllowed(t *testing.T) {
	store := newTestStore(t)
	a := newAPIServer(func() bridgeClient { return &fakeClient{connected: true} }, store, "", "bridge.lan:9000")
	h := a.handler()
	ok := func(r *http.Request) { r.Host = "bridge.lan:9000" }
	if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"1","message":"x"}`, ok); rec.Code != http.StatusOK {
		t.Fatalf("configured host: code = %d", rec.Code)
	}
	other := func(r *http.Request) { r.Host = "other.lan:9000" }
	if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"1","message":"x"}`, other); rec.Code != http.StatusForbidden {
		t.Fatalf("other host: code = %d", rec.Code)
	}
	// A wildcard bind does not whitelist anything extra
	if a2 := newAPIServer(nil, store, "", "0.0.0.0:8080"); len(a2.extraHosts) != 0 {
		t.Fatalf("wildcard host whitelisted: %v", a2.extraHosts)
	}
}

func TestToken(t *testing.T) {
	const body = `{"recipient":"1","message":"hi"}`
	bearer := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", v) }
	}
	tests := []struct {
		name  string
		token string
		opts  []func(*http.Request)
		want  int
	}{
		{"unset token, no header", "", nil, http.StatusOK},
		{"missing", "s3cret", nil, http.StatusUnauthorized},
		{"wrong", "s3cret", []func(*http.Request){bearer("Bearer nope")}, http.StatusUnauthorized},
		{"wrong scheme", "s3cret", []func(*http.Request){bearer("Basic s3cret")}, http.StatusUnauthorized},
		{"prefix of token", "s3cret", []func(*http.Request){bearer("Bearer s3c")}, http.StatusUnauthorized},
		{"right", "s3cret", []func(*http.Request){bearer("Bearer s3cret")}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, h := newTestAPI(t, &fakeClient{connected: true}, tt.token)
			if rec := do(h, http.MethodPost, "/api/send", body, tt.opts...); rec.Code != tt.want {
				t.Fatalf("code = %d want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestPerRouteAuthFlag(t *testing.T) {
	// Routes marked auth:false stay open even when a token is configured (used by health in a later task)
	store := newTestStore(t)
	a := newAPIServer(func() bridgeClient { return nil }, store, "tok", "127.0.0.1:8080")
	hit := false
	mux := http.NewServeMux()
	mux.Handle("/open", a.guard(false, func(w http.ResponseWriter, _ *http.Request) { hit = true }))
	mux.Handle("/closed", a.guard(true, func(w http.ResponseWriter, _ *http.Request) {}))
	if rec := do(mux, http.MethodGet, "/open", ""); rec.Code != http.StatusOK || !hit {
		t.Fatalf("open route: code = %d hit=%v", rec.Code, hit)
	}
	if rec := do(mux, http.MethodGet, "/closed", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("closed route: code = %d", rec.Code)
	}
	// Host/Origin checks still apply to open routes
	if rec := do(mux, http.MethodGet, "/open", "", func(r *http.Request) { r.Header.Set("Origin", "x") }); rec.Code != http.StatusForbidden {
		t.Fatalf("open route with Origin: code = %d", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	_, h := newTestAPI(t, &fakeClient{connected: true}, "")
	big := `{"recipient":"1","message":"` + strings.Repeat("a", maxRequestBody) + `"}`
	if rec := do(h, http.MethodPost, "/api/send", big); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: code = %d want 400", rec.Code)
	}
}

// seedMedia stores a media message whose (remote-controlled) filename is name.
func seedMedia(t *testing.T, s *MessageStore, chat, id, name string) {
	t.Helper()
	if err := s.StoreChat(chat, "c", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage(id, chat, "sender", "", time.Now(), false, "document", name, "https://mmg.whatsapp.net/v/x.enc?a=b",
		[]byte{1}, []byte{2}, []byte{3}, 9); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadTraversalStaysInStore(t *testing.T) {
	names := []string{"../../x", "../../../.zshrc", "/etc/passwd", `..\..\evil`, "a/b/c.txt", "..", "."}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store := newTestStoreIn(t, root)
			chat := "1234@s.whatsapp.net"
			seedMedia(t, store, chat, "m1", name)
			fc := &fakeClient{connected: true, data: []byte("payload")}

			ok, _, fn, path, err := downloadMedia(fc, store, "m1", chat)
			if name == ".." || name == "." {
				if ok || err == nil {
					t.Fatalf("expected rejection, got ok=%v err=%v", ok, err)
				}
				if fc.downloads != 0 {
					t.Fatalf("rejected filename must not download")
				}
				return
			}
			if !ok || err != nil {
				t.Fatalf("download failed: ok=%v err=%v", ok, err)
			}
			assertUnder(t, root, path)
			if strings.ContainsAny(fn, `/\`) {
				t.Fatalf("returned filename %q still has separators", fn)
			}
			if got, _ := os.ReadFile(path); string(got) != "payload" {
				t.Fatalf("file content = %q", got)
			}
			// Nothing may have been written outside root (parent dir of the store)
			entries, _ := os.ReadDir(filepath.Dir(root))
			for _, e := range entries {
				if e.Name() == "x" || e.Name() == ".zshrc" || e.Name() == "evil" {
					t.Fatalf("escaped file %s created next to store", e.Name())
				}
			}
		})
	}
}

func TestDownloadExistingFileEarlyReturnStaysInStore(t *testing.T) {
	root := t.TempDir()
	store := newTestStoreIn(t, root)
	chat := "1234@s.whatsapp.net"
	seedMedia(t, store, chat, "m1", "../../secret.txt")
	// A file at the escaped location must NOT be returned as the cached download
	outside := filepath.Join(filepath.Dir(filepath.Dir(root)), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Skipf("cannot write outside temp root: %v", err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	fc := &fakeClient{connected: true, data: []byte("inside")}
	ok, _, _, path, err := downloadMedia(fc, store, "m1", chat)
	if !ok || err != nil {
		t.Fatalf("download failed: %v", err)
	}
	assertUnder(t, root, path)
	if got, _ := os.ReadFile(path); string(got) != "inside" {
		t.Fatalf("early return served outside file: %q", got)
	}

	// Second call hits the existing-file early return, still under the store, no new download
	before := fc.downloads
	ok, _, _, path2, err := downloadMedia(fc, store, "m1", chat)
	if !ok || err != nil || path2 != path || fc.downloads != before {
		t.Fatalf("early return: ok=%v err=%v path=%q downloads=%d", ok, err, path2, fc.downloads)
	}
}

func TestDownloadChatDirSanitized(t *testing.T) {
	root := t.TempDir()
	store := newTestStoreIn(t, root)
	chat := "../../evil@s.whatsapp.net"
	seedMedia(t, store, chat, "m1", "f.txt")
	fc := &fakeClient{connected: true, data: []byte("d")}
	ok, _, _, path, err := downloadMedia(fc, store, "m1", chat)
	if !ok || err != nil {
		t.Fatalf("download failed: %v", err)
	}
	assertUnder(t, root, path)
	if filepath.Dir(path) != filepath.Join(mustAbs(t, root), ".._.._evil@s.whatsapp.net") {
		t.Fatalf("unexpected chat dir %q", filepath.Dir(path))
	}
	if got := sanitizeChatDirName(".."); got != "" {
		t.Fatalf("'..' chat dir accepted as %q", got)
	}
}

func TestDownloadHandlerEndToEnd(t *testing.T) {
	root := t.TempDir()
	store := newTestStoreIn(t, root)
	seedMedia(t, store, "1234@s.whatsapp.net", "m1", "../../x")
	fc := &fakeClient{connected: true, data: []byte("d")}
	a := newAPIServer(func() bridgeClient { return fc }, store, "", "127.0.0.1:8080")
	rec := do(a.handler(), http.MethodPost, "/api/download", `{"message_id":"m1","chat_jid":"1234@s.whatsapp.net"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp DownloadMediaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success || resp.Filename != "x" {
		t.Fatalf("resp = %+v err=%v", resp, err)
	}
	assertUnder(t, root, resp.Path)

	// Validation paths
	for body, want := range map[string]int{"{": 400, `{"message_id":"m1"}`: 400} {
		if rec := do(a.handler(), http.MethodPost, "/api/download", body); rec.Code != want {
			t.Errorf("body %q: code = %d want %d", body, rec.Code, want)
		}
	}
	if rec := do(a.handler(), http.MethodGet, "/api/download", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: code = %d", rec.Code)
	}
}

func TestBridgeAddrDefaultsToLoopback(t *testing.T) {
	t.Setenv("WHATSAPP_BRIDGE_ADDR", "")
	addr := bridgeAddr()
	if addr != "127.0.0.1:8080" {
		t.Fatalf("default addr = %q", addr)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("default host %q is not loopback (err=%v)", host, err)
	}
	t.Setenv("WHATSAPP_BRIDGE_ADDR", "127.0.0.1:0")
	if got := bridgeAddr(); got != "127.0.0.1:0" {
		t.Fatalf("env override = %q", got)
	}
}

func TestStartRESTServerBindsLoopbackAndShutsDown(t *testing.T) {
	// Random free port on the default (loopback) host; never the real 8080
	host, _, _ := net.SplitHostPort(defaultBridgeAddr)
	store := newTestStore(t)
	a := newAPIServer(func() bridgeClient { return &fakeClient{} }, store, "", host+":0")
	srv, bound, err := startRESTServer(host+":0", a.handler())
	if err != nil {
		t.Fatal(err)
	}
	tcp := bound.(*net.TCPAddr)
	if !tcp.IP.IsLoopback() || tcp.Port == 0 {
		t.Fatalf("bound to %v, want loopback with a real port", tcp)
	}

	url := "http://" + tcp.String() + "/api/send"
	httpc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := httpc.Post(url, "application/json", bytes.NewBufferString(`{"recipient":"1","message":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError { // "Not connected"
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Real HTTP keeps the guards: text/plain is refused
	resp, err = httpc.Post(url, "text/plain", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain status = %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := net.DialTimeout("tcp", tcp.String(), time.Second); err == nil {
		t.Fatal("port still accepting after shutdown")
	}
}

func TestStartRESTServerBindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, _, err := startRESTServer(ln.Addr().String(), http.NewServeMux()); err == nil {
		t.Fatal("expected bind error for a port in use")
	}
}

func newTestStoreIn(t *testing.T, dir string) *MessageStore {
	t.Helper()
	t.Setenv("WHATSAPP_STORE_DIR", dir)
	s, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// assertUnder fails unless path is lexically and physically under root.
func assertUnder(t *testing.T, root, path string) {
	t.Helper()
	absRoot, err := filepath.EvalSymlinks(mustAbs(t, root))
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	rel, err := filepath.Rel(absRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("%s is not under %s", real, absRoot)
	}
}
