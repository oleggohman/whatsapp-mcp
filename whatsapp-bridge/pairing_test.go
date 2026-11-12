package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

// fakeWA implements waClient without any network access.
type fakeWA struct {
	*fakeClient
	mu          sync.Mutex
	paired      bool
	conn        bool
	order       []string
	connects    int
	disconnects int
	qrCalls     int
	qrCh        chan whatsmeow.QRChannelItem
	autoCode    bool // GetQRChannel pre-queues a first code item
	qrErr       error
	connectErr  error
	pairCode    string
	pairErr     error
	pairArgs    []string
	qrCtx       context.Context // ctx passed to the last GetQRChannel
	connGate    chan struct{}   // when set, IsConnected blocks until it is closed
	connEntered chan struct{}   // when set, IsConnected signals here before blocking
}

func newFakeWA(paired bool) *fakeWA {
	return &fakeWA{fakeClient: &fakeClient{}, paired: paired, conn: paired, pairCode: "ABCD-EFGH"}
}

func (f *fakeWA) IsConnected() bool {
	f.mu.Lock()
	gate, entered := f.connGate, f.connEntered
	f.mu.Unlock()
	if gate != nil {
		// Like whatsmeow's socketLock held by a slow Connect.
		if entered != nil {
			entered <- struct{}{}
		}
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conn
}
func (f *fakeWA) Paired() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.paired }
func (f *fakeWA) Connect() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "connect")
	f.connects++
	if f.connectErr != nil {
		return f.connectErr
	}
	f.conn = true
	return nil
}
func (f *fakeWA) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects++
	f.conn = false
}
func (f *fakeWA) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qrCtx = ctx
	f.order = append(f.order, "qr")
	f.qrCalls++
	if f.qrErr != nil {
		return nil, f.qrErr
	}
	f.qrCh = make(chan whatsmeow.QRChannelItem, 8)
	if f.autoCode {
		f.qrCh <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "first-code", Timeout: 20 * time.Second}
	}
	return f.qrCh, nil
}
func (f *fakeWA) PairPhone(_ context.Context, phone string, push bool, ct whatsmeow.PairClientType, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairArgs = []string{phone, string(ct), name}
	if !push {
		f.pairArgs = append(f.pairArgs, "nopush")
	}
	return f.pairCode, f.pairErr
}
func (f *fakeWA) send(item whatsmeow.QRChannelItem) {
	f.mu.Lock()
	ch := f.qrCh
	f.mu.Unlock()
	ch <- item
}
func (f *fakeWA) counts() (connects, disconnects, qrCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connects, f.disconnects, f.qrCalls
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func codeItem(c string) whatsmeow.QRChannelItem {
	return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: c, Timeout: 20 * time.Second}
}

// newTestPairing builds a manager whose logout replacement is returned by next.
func newTestPairing(c *fakeWA, next *fakeWA) *pairingManager {
	pm := newPairingManager(c, func() (waClient, error) {
		if next == nil {
			return nil, errors.New("no replacement client in this test")
		}
		return next, nil
	})
	pm.waitCode = 500 * time.Millisecond
	return pm
}

func newPairAPI(t *testing.T, pm *pairingManager, token string) http.Handler {
	t.Helper()
	t.Setenv("WHATSAPP_STORE_DIR", t.TempDir())
	a := newAPIServer(pm.Client, newTestStore(t), token, "127.0.0.1:8080")
	a.pairing = pm
	return a.handler()
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("bad JSON %q: %v", body, err)
	}
	return v
}

func TestStatusTransitions(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	if st := pm.Status(); st.Pairing != "idle" || st.LoggedIn || st.Connected {
		t.Fatalf("initial status = %+v", st)
	}
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	if st := pm.Status(); st.Pairing != "qr" || st.QR != "" {
		t.Fatalf("armed status = %+v", st)
	}
	fw.send(codeItem("code-1"))
	waitFor(t, "first QR", func() bool { return pm.Status().QR == "code-1" })
	st := pm.Status()
	if st.Pairing != "qr" || st.QRExpiresAt <= time.Now().Unix() {
		t.Fatalf("status with code = %+v", st)
	}
	fw.send(codeItem("code-2"))
	waitFor(t, "rotated QR", func() bool { return pm.Status().QR == "code-2" })
	// GetQRChannel must come before Connect
	if len(fw.order) != 2 || fw.order[0] != "qr" || fw.order[1] != "connect" {
		t.Fatalf("call order = %v", fw.order)
	}
	// A second start while a session is active is a no-op
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	if c, _, q := fw.counts(); c != 1 || q != 1 {
		t.Fatalf("connects=%d qrCalls=%d, want 1/1", c, q)
	}
}

func TestQRTimeoutGoesIdleAndRearms(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	fw.send(codeItem("c"))
	waitFor(t, "QR", func() bool { return pm.Status().QR == "c" })
	fw.send(whatsmeow.QRChannelTimeout)
	waitFor(t, "idle after timeout", func() bool { return pm.Status().Pairing == "idle" })
	if _, d, _ := fw.counts(); d != 1 {
		t.Fatalf("disconnects = %d, want 1", d)
	}
	if st := pm.Status(); st.QR != "" || st.LoggedIn {
		t.Fatalf("status after timeout = %+v", st)
	}
	// Re-arm on demand through the API
	h := newPairAPI(t, pm, "")
	rec := do(h, http.MethodPost, "/api/start-pairing", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if st := decode[statusResponse](t, rec.Body.String()); st.Pairing != "qr" {
		t.Fatalf("after re-arm: %+v", st)
	}
	if c, _, q := fw.counts(); c != 2 || q != 2 {
		t.Fatalf("connects=%d qrCalls=%d, want 2/2", c, q)
	}
	// An error item also ends the session
	fw.send(whatsmeow.QRChannelItem{Event: "error", Error: context.Canceled})
	waitFor(t, "idle after error", func() bool { return pm.Status().Pairing == "idle" })
}

func TestQRSuccessWithOpenChannel(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	fw.send(codeItem("c"))
	waitFor(t, "QR", func() bool { return pm.Status().QR == "c" })
	// The channel stays open (whatsmeow closes it later); success alone must flip the state
	fw.send(whatsmeow.QRChannelSuccess)
	waitFor(t, "logged_in", func() bool { return pm.Status().LoggedIn })
	st := pm.Status()
	if st.Pairing != "idle" || st.QR != "" {
		t.Fatalf("status after success = %+v", st)
	}
	if _, d, _ := fw.counts(); d != 0 {
		t.Fatalf("must not disconnect after success, got %d", d)
	}
	// API is ready: status answers and further pairing is refused
	h := newPairAPI(t, pm, "")
	if rec := do(h, http.MethodPost, "/api/start-pairing", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if _, err := pm.StartPairing(); err != errAlreadyPaired {
		t.Fatalf("StartPairing after success: %v", err)
	}
}

func TestPairSuccessEvent(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	pm.handleEvent(fw, &events.PairSuccess{})
	if st := pm.Status(); !st.LoggedIn || st.Pairing != "idle" {
		t.Fatalf("status = %+v", st)
	}
}

// whatsmeow's QR emitter disconnects the client when the GetQRChannel ctx is cancelled before
// it saw PairSuccess, so the event path must not cancel it; consumeQR does after the close.
func TestPairSuccessEventKeepsQRContextUntilChannelCloses(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	fw.mu.Lock()
	ctx, ch := fw.qrCtx, fw.qrCh
	fw.mu.Unlock()
	pm.handleEvent(fw, &events.PairSuccess{})
	if err := ctx.Err(); err != nil {
		t.Fatalf("PairSuccess cancelled the QR channel context: %v", err)
	}
	ch <- whatsmeow.QRChannelSuccess
	close(ch)
	waitFor(t, "QR context cancelled after the channel closed", func() bool { return ctx.Err() != nil })
	if _, d, _ := fw.counts(); d != 0 {
		t.Fatalf("must not disconnect after success, got %d", d)
	}
}

// A Connect holding whatsmeow's socket lock blocks IsConnected; that must not block the
// pairing manager's mutex (Client, events) while Status or Reconnect waits.
func TestSlowIsConnectedDoesNotBlockManager(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(pm *pairingManager)
	}{
		{"Status", func(pm *pairingManager) { pm.Status() }},
		{"Reconnect", func(pm *pairingManager) { _ = pm.Reconnect() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw := newFakeWA(true)
			pm := newTestPairing(fw, nil)
			gate, entered := make(chan struct{}), make(chan struct{}, 1)
			fw.mu.Lock()
			fw.connGate, fw.connEntered = gate, entered
			fw.mu.Unlock()
			done := make(chan struct{})
			go func() { tc.call(pm); close(done) }()
			<-entered // the call is now blocked inside IsConnected
			free := make(chan struct{})
			go func() {
				pm.Client()
				pm.handleEvent(fw, &events.Connected{})
				close(free)
			}()
			select {
			case <-free:
			case <-time.After(time.Second):
				close(gate)
				t.Fatalf("%s held the manager lock while IsConnected blocked", tc.name)
			}
			close(gate)
			<-done
		})
	}
}

func TestLogoutSwapsClient(t *testing.T) {
	old := newFakeWA(true)
	fresh := newFakeWA(false)
	pm := newTestPairing(old, fresh)
	if pm.Client() != bridgeClient(old) {
		t.Fatal("holder should start with the old client")
	}
	if !pm.Status().LoggedIn {
		t.Fatal("paired store should start logged in")
	}
	pm.handleEvent(old, &events.LoggedOut{})
	if pm.Client() != bridgeClient(fresh) {
		t.Fatal("holder should hold the new client after logout")
	}
	if st := pm.Status(); st.LoggedIn || st.Pairing != "idle" {
		t.Fatalf("status after logout = %+v", st)
	}
	waitFor(t, "old client disconnect", func() bool { _, d, _ := old.counts(); return d == 1 })
	// Events from the stale client no longer matter
	pm.handleEvent(old, &events.Connected{})
	if pm.Status().LoggedIn {
		t.Fatal("stale event changed state")
	}
	// New client can pair
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	if _, _, q := fresh.counts(); q != 1 {
		t.Fatalf("fresh qrCalls = %d", q)
	}
}

func TestStreamReplacedNoAutoReconnect(t *testing.T) {
	fw := newFakeWA(true)
	pm := newTestPairing(fw, nil)
	pm.handleEvent(fw, &events.StreamReplaced{})
	fw.Disconnect() // whatsmeow drops the socket itself
	st := pm.Status()
	if st.Pairing != "replaced" || st.Connected {
		t.Fatalf("status = %+v", st)
	}
	time.Sleep(30 * time.Millisecond)
	if c, _, _ := fw.counts(); c != 0 {
		t.Fatalf("Connect called %d times after StreamReplaced", c)
	}
	// Only an explicit request reconnects
	h := newPairAPI(t, pm, "")
	rec := do(h, http.MethodPost, "/api/reconnect", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if c, _, _ := fw.counts(); c != 1 {
		t.Fatalf("Connect calls = %d, want 1", c)
	}
	if st := decode[statusResponse](t, rec.Body.String()); st.Pairing != "idle" || !st.Connected {
		t.Fatalf("status after reconnect = %+v", st)
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in, want string
		bad      bool
	}{
		{"+1 (415) 555-2671", "14155552671", false},
		{"491701234567", "491701234567", false},
		{"0491701234567", "", true},
		{"12345", "", true},
		{"123456", "", true},
		{"", "", true},
		{"abc", "", true},
		{"1234567890123456", "", true},
	}
	for _, c := range cases {
		got, err := normalizePhone(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("normalizePhone(%q) = %q, %v", c.in, got, err)
		}
	}
}

func TestPairPhoneAPI(t *testing.T) {
	post := func(h http.Handler, body string) (int, pairPhoneResponse) {
		rec := do(h, http.MethodPost, "/api/pair-phone", body)
		var r pairPhoneResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return rec.Code, r
	}

	t.Run("validation", func(t *testing.T) {
		pm := newTestPairing(newFakeWA(false), nil)
		h := newPairAPI(t, pm, "")
		for _, body := range []string{`{"phone":"0123456789"}`, `{"phone":"123"}`, `{"phone":""}`, `{}`} {
			if code, _ := post(h, body); code != http.StatusBadRequest {
				t.Errorf("%s -> %d, want 400", body, code)
			}
		}
		if rec := do(h, http.MethodPost, "/api/pair-phone", `not json`); rec.Code != http.StatusBadRequest {
			t.Errorf("bad JSON -> %d", rec.Code)
		}
		if rec := do(h, http.MethodGet, "/api/pair-phone", ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET -> %d", rec.Code)
		}
	})

	t.Run("already logged in", func(t *testing.T) {
		pm := newTestPairing(newFakeWA(true), nil)
		h := newPairAPI(t, pm, "")
		if code, r := post(h, `{"phone":"14155552671"}`); code != http.StatusConflict || r.Success {
			t.Errorf("got %d %+v", code, r)
		}
	})

	t.Run("no session starts one", func(t *testing.T) {
		fw := newFakeWA(false)
		fw.autoCode = true
		pm := newTestPairing(fw, nil)
		h := newPairAPI(t, pm, "")
		code, r := post(h, `{"phone":"+1 415 555 2671"}`)
		if code != http.StatusOK || !r.Success || r.Code != "ABCD-EFGH" {
			t.Fatalf("got %d %+v", code, r)
		}
		if len(fw.pairArgs) != 3 || fw.pairArgs[0] != "14155552671" || fw.pairArgs[1] != "1" || fw.pairArgs[2] != "Chrome (Linux)" {
			t.Errorf("PairPhone args = %v", fw.pairArgs)
		}
		if st := pm.Status(); st.Pairing != "code" {
			t.Errorf("status = %+v", st)
		}
	})

	t.Run("no first code times out", func(t *testing.T) {
		fw := newFakeWA(false)
		pm := newTestPairing(fw, nil)
		pm.waitCode = 40 * time.Millisecond
		h := newPairAPI(t, pm, "")
		if code, _ := post(h, `{"phone":"14155552671"}`); code != http.StatusGatewayTimeout {
			t.Errorf("got %d, want 504", code)
		}
	})

	t.Run("session start failure", func(t *testing.T) {
		fw := newFakeWA(false)
		fw.qrErr = whatsmeow.ErrQRAlreadyConnected
		pm := newTestPairing(fw, nil)
		h := newPairAPI(t, pm, "")
		if code, r := post(h, `{"phone":"14155552671"}`); code != http.StatusBadGateway || r.Message == "" {
			t.Errorf("got %d %+v", code, r)
		}
	})

	t.Run("whatsmeow errors are mapped", func(t *testing.T) {
		for _, c := range []struct {
			err  error
			want int
		}{
			{whatsmeow.ErrIQRateOverLimit, http.StatusTooManyRequests},
			{whatsmeow.ErrIQBadRequest, http.StatusBadRequest},
			{whatsmeow.ErrIQTimedOut, http.StatusGatewayTimeout},
			{whatsmeow.ErrNotConnected, http.StatusBadGateway},
			{os.ErrInvalid, http.StatusBadGateway},
		} {
			fw := newFakeWA(false)
			fw.autoCode = true
			fw.pairErr = c.err
			h := newPairAPI(t, newTestPairing(fw, nil), "")
			if code, r := post(h, `{"phone":"14155552671"}`); code != c.want || r.Success || r.Message == "" {
				t.Errorf("%v -> %d %+v, want %d", c.err, code, r, c.want)
			}
		}
	})
}

func TestHealthAndStatusAuth(t *testing.T) {
	dir := t.TempDir()
	pm := newTestPairing(newFakeWA(false), nil)
	a := newAPIServer(pm.Client, newTestStoreIn(t, dir), "sekret", "127.0.0.1:8080")
	a.pairing = pm
	h := a.handler()

	rec := do(h, http.MethodGet, "/api/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	hr := decode[healthResponse](t, rec.Body.String())
	abs, _ := filepath.Abs(dir)
	if hr.Service != "whatsapp-bridge" || hr.StoreDir != abs || hr.PID != os.Getpid() || !hr.AuthRequired || hr.Version == "" {
		t.Fatalf("health = %+v (want store_dir %s)", hr, abs)
	}

	// bridgeVersion is a var so the supervisor can stamp it with -ldflags; health reports it.
	old := bridgeVersion
	bridgeVersion = "stamped-abc123"
	t.Cleanup(func() { bridgeVersion = old })
	rec = do(h, http.MethodGet, "/api/health", "")
	if hr := decode[healthResponse](t, rec.Body.String()); hr.Version != "stamped-abc123" {
		t.Fatalf("health version = %q, want stamped value", hr.Version)
	}

	if rec := do(h, http.MethodGet, "/api/status", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want 401", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/pair-phone", `{"phone":"14155552671"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("pair-phone without token = %d, want 401", rec.Code)
	}
	// GET carries no Content-Type and must still pass the guard
	rec = do(h, http.MethodGet, "/api/status", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer sekret") })
	if rec.Code != http.StatusOK {
		t.Fatalf("status with token = %d %s", rec.Code, rec.Body)
	}
}

func TestHealthWithoutToken(t *testing.T) {
	t.Setenv("WHATSAPP_STORE_DIR", t.TempDir())
	a := newAPIServer(func() bridgeClient { return nil }, newTestStore(t), "", "127.0.0.1:8080")
	rec := do(a.handler(), http.MethodGet, "/api/health", "")
	if hr := decode[healthResponse](t, rec.Body.String()); rec.Code != http.StatusOK || hr.AuthRequired {
		t.Fatalf("health = %d %+v", rec.Code, hr)
	}
	// No pairing manager wired: status answers 503 rather than panicking
	if rec := do(a.handler(), http.MethodGet, "/api/status", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status without manager = %d", rec.Code)
	}
}

func TestInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.lock")
	l1, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(path); err != errLockHeld {
		t.Fatalf("second acquire = %v, want errLockHeld", err)
	}
	l1.Release()
	l3, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l3.Release()
}

func TestRunExitsWith75WhenLockHeld(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WHATSAPP_STORE_DIR", dir)
	// Keep the REST port out of play even if the lock check were skipped
	t.Setenv("WHATSAPP_BRIDGE_ADDR", "127.0.0.1:0")
	l, err := acquireLock(filepath.Join(dir, "bridge.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if code := run(); code != 75 {
		t.Fatalf("run() = %d, want 75", code)
	}
}

func TestTokenPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
	if tok, err := loadToken(dir); err != nil || tok != "" {
		t.Fatalf("no token: %q %v", tok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bridge.token"), []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, _ := loadToken(dir); tok != "from-file" {
		t.Fatalf("file token = %q", tok)
	}
	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "from-env")
	if tok, _ := loadToken(dir); tok != "from-env" {
		t.Fatalf("env must win, got %q", tok)
	}
}

func TestEmptyTokenFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
	if err := os.WriteFile(filepath.Join(dir, "bridge.token"), []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := loadToken(dir); err == nil {
		t.Fatalf("empty token file must be an error, got token %q", tok)
	}
	// An explicit env token still wins over a broken file.
	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "from-env")
	if tok, err := loadToken(dir); err != nil || tok != "from-env" {
		t.Fatalf("env token: %q %v", tok, err)
	}
}

func TestIdleExit(t *testing.T) {
	t.Run("fires when enabled", func(t *testing.T) {
		tr := newIdleTracker()
		fired := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		watchIdle(ctx, tr, 40*time.Millisecond, func() { fired <- struct{}{} })
		select {
		case <-fired:
		case <-time.After(2 * time.Second):
			t.Fatal("idle exit did not fire")
		}
	})
	t.Run("disabled never fires", func(t *testing.T) {
		tr := newIdleTracker()
		fired := make(chan struct{}, 1)
		watchIdle(context.Background(), tr, 0, func() { fired <- struct{}{} })
		select {
		case <-fired:
			t.Fatal("fired while disabled")
		case <-time.After(60 * time.Millisecond):
		}
	})
	t.Run("health does not reset the timer but other requests do", func(t *testing.T) {
		t.Setenv("WHATSAPP_STORE_DIR", t.TempDir())
		pm := newTestPairing(newFakeWA(false), nil)
		a := newAPIServer(pm.Client, newTestStore(t), "", "127.0.0.1:8080")
		a.pairing = pm
		a.idle = newIdleTracker()
		h := a.handler()
		time.Sleep(60 * time.Millisecond)
		do(h, http.MethodGet, "/api/health", "")
		if a.idle.idleFor() < 60*time.Millisecond {
			t.Fatal("health reset the idle timer")
		}
		do(h, http.MethodGet, "/api/status", "")
		if a.idle.idleFor() >= 60*time.Millisecond {
			t.Fatal("status did not reset the idle timer")
		}
	})
	t.Run("env parsing", func(t *testing.T) {
		t.Setenv("WHATSAPP_BRIDGE_IDLE_EXIT", "")
		if idleExitDuration() != 0 {
			t.Fatal("default must be off")
		}
		t.Setenv("WHATSAPP_BRIDGE_IDLE_EXIT", "90s")
		if idleExitDuration() != 90*time.Second {
			t.Fatal("90s not parsed")
		}
		t.Setenv("WHATSAPP_BRIDGE_IDLE_EXIT", "bogus")
		if idleExitDuration() != 0 {
			t.Fatal("invalid must disable")
		}
	})
}

// A cross-site <img> or navigation GET carries no Origin and, without a token, no auth:
// GET /api/status must never start pairing or reconnect. The POST replacements are guarded.
func TestStatusGETCannotChangeState(t *testing.T) {
	unpaired := newFakeWA(false)
	pm := newTestPairing(unpaired, nil)
	h := newPairAPI(t, pm, "")
	for _, q := range []string{"?start_pairing=1", "?reconnect=1", "?start_pairing=1&reconnect=1"} {
		if rec := do(h, http.MethodGet, "/api/status"+q, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET /api/status%s = %d, want 400", q, rec.Code)
		}
	}
	if c, _, qr := unpaired.counts(); c != 0 || qr != 0 {
		t.Fatalf("GET changed state: connects=%d qrCalls=%d", c, qr)
	}
	for _, p := range []string{"/api/start-pairing", "/api/reconnect"} {
		if rec := do(h, http.MethodGet, p, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405", p, rec.Code)
		}
		// Browser form posts: wrong content type, or any Origin, are refused before the handler
		if rec := do(h, http.MethodPost, p, "", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }); rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("text/plain POST %s = %d, want 415", p, rec.Code)
		}
		if rec := do(h, http.MethodPost, p, "{}", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin POST %s = %d, want 403", p, rec.Code)
		}
	}
	if c, _, qr := unpaired.counts(); c != 0 || qr != 0 {
		t.Fatalf("rejected POSTs changed state: connects=%d qrCalls=%d", c, qr)
	}
	// Plain GET status still works
	if rec := do(h, http.MethodGet, "/api/status", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d", rec.Code)
	}
	// Reconnect on an unpaired client is a 409, start-pairing arms a session
	if rec := do(h, http.MethodPost, "/api/reconnect", "{}"); rec.Code != http.StatusConflict {
		t.Fatalf("POST /api/reconnect unpaired = %d, want 409", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/start-pairing", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("POST /api/start-pairing = %d %s", rec.Code, rec.Body)
	}
	if _, _, qr := unpaired.counts(); qr != 1 {
		t.Fatalf("start-pairing qrCalls = %d, want 1", qr)
	}
}

// GetQRChannel leaves an event handler behind when Connect fails; the retry must use a
// fresh client so that stale handler cannot disconnect the new session.
func TestFailedConnectReplacesClient(t *testing.T) {
	first := newFakeWA(false)
	first.connectErr = errors.New("dial failed")
	fresh := newFakeWA(false)
	pm := newTestPairing(first, fresh)

	if _, err := pm.StartPairing(); err == nil {
		t.Fatal("StartPairing should fail when Connect fails")
	}
	if pm.current() != waClient(fresh) {
		t.Fatal("client with the stale QR handler was kept after a failed connect")
	}
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	if c, _, q := first.counts(); c != 1 || q != 1 {
		t.Fatalf("old client reused: connects=%d qrCalls=%d", c, q)
	}
	if c, _, q := fresh.counts(); c != 1 || q != 1 {
		t.Fatalf("fresh client: connects=%d qrCalls=%d, want 1/1", c, q)
	}
	fresh.send(codeItem("c"))
	waitFor(t, "QR on the fresh client", func() bool { return pm.Status().QR == "c" })
}

// Without a replacement the old client stays (pairing can still be retried).
func TestFailedConnectKeepsClientWhenReplacementFails(t *testing.T) {
	first := newFakeWA(false)
	first.connectErr = errors.New("dial failed")
	pm := newTestPairing(first, nil)
	if _, err := pm.StartPairing(); err == nil {
		t.Fatal("StartPairing should fail when Connect fails")
	}
	if pm.current() != waClient(first) {
		t.Fatal("client must stay when no replacement can be built")
	}
}

// A timeout teardown that runs while StartPairing is arming a new session on the same
// client must not disconnect that new session.
func TestQRTeardownDoesNotKillNewSession(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	s, err := pm.StartPairing()
	if err != nil {
		t.Fatal(err)
	}

	pm.startMu.Lock() // a StartPairing is in progress
	done := make(chan struct{})
	go func() {
		pm.handleQR(s, whatsmeow.QRChannelTimeout)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	// StartPairing finishes arming the next session before releasing startMu.
	s2 := &pairSession{client: fw, cancel: func() {}, firstCode: make(chan struct{})}
	pm.mu.Lock()
	pm.sess = s2
	pm.mu.Unlock()
	pm.startMu.Unlock()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handleQR did not return")
	}
	if _, d, _ := fw.counts(); d != 0 {
		t.Fatalf("stale teardown disconnected the new session (disconnects=%d)", d)
	}
	pm.mu.Lock()
	active := pm.sess == s2
	pm.mu.Unlock()
	if !active {
		t.Fatal("stale teardown ended the new session")
	}
}
