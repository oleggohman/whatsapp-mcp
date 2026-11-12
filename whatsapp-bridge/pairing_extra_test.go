package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestRealClientPaired(t *testing.T) {
	dev := &store.Device{}
	rc := realClient{whatsmeow.NewClient(dev, nil)}
	if rc.Paired() {
		t.Fatal("a device without an ID is not paired")
	}
	jid := types.NewJID("15550100001", types.DefaultUserServer)
	dev.ID = &jid
	if !rc.Paired() {
		t.Fatal("a device with an ID is paired")
	}
}

func TestStderrIsTTY(t *testing.T) {
	orig := os.Stderr
	t.Cleanup(func() { os.Stderr = orig })

	devNull, err := os.Open(os.DevNull) // a character device, like a terminal
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	os.Stderr = devNull
	if !stderrIsTTY() {
		t.Error("a character device counts as a terminal")
	}

	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	os.Stderr = wr
	if stderrIsTTY() {
		t.Error("a pipe is not a terminal")
	}
	wr.Close()
	if stderrIsTTY() { // Stat on a closed file fails
		t.Error("a closed stderr is not a terminal")
	}
}

func TestStartPairingDropsAttemptWhenClientReplacedWhileConnecting(t *testing.T) {
	fw, next := newFakeWA(false), newFakeWA(false)
	pm := newTestPairing(fw, next)
	fw.onConnect = func() { pm.handleEvent(fw, &events.LoggedOut{}) }
	_, err := pm.StartPairing()
	if err == nil || !strings.Contains(err.Error(), "client was replaced") {
		t.Fatalf("err = %v", err)
	}
	if pm.current() != waClient(next) {
		t.Fatal("the replacement client must stay current")
	}
	waitFor(t, "both disconnects (logout + dropped attempt)", func() bool { _, d, _ := fw.counts(); return d == 2 })
	if st := pm.Status(); st.Pairing != pairIdle {
		t.Fatalf("pairing = %q", st.Pairing)
	}
}

func TestReplaceUnpairedClientGuards(t *testing.T) {
	t.Run("not current", func(t *testing.T) {
		fw, other, next := newFakeWA(false), newFakeWA(false), newFakeWA(false)
		pm := newTestPairing(fw, next)
		pm.replaceUnpairedClient(other)
		if pm.current() != waClient(fw) {
			t.Fatal("client must stay")
		}
	})
	t.Run("no factory", func(t *testing.T) {
		fw := newFakeWA(false)
		pm := newPairingManager(fw, nil)
		pm.replaceUnpairedClient(fw)
		if pm.current() != waClient(fw) {
			t.Fatal("client must stay")
		}
	})
	t.Run("paired", func(t *testing.T) {
		fw, next := newFakeWA(true), newFakeWA(false)
		pm := newTestPairing(fw, next)
		pm.replaceUnpairedClient(fw)
		if pm.current() != waClient(fw) {
			t.Fatal("a paired client must never be replaced")
		}
	})
	t.Run("factory fails", func(t *testing.T) {
		fw := newFakeWA(false)
		pm := newTestPairing(fw, nil)
		out := captureStderr(t, func() { pm.replaceUnpairedClient(fw) })
		if pm.current() != waClient(fw) || !strings.Contains(out, "Failed to create a new client after a failed pairing connect") {
			t.Fatalf("log = %q", out)
		}
	})
}

func TestHandleQRPrintsCodesAndKeepsSessionOnPasskeyEvents(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	printed := make(chan string, 4)
	pm.printQR = func(code string) { printed <- code }
	if _, err := pm.StartPairing(); err != nil {
		t.Fatal(err)
	}
	fw.send(codeItem("qr-1"))
	select {
	case got := <-printed:
		if got != "qr-1" {
			t.Fatalf("printed %q", got)
		}
	case <-time.After(settleTimeout):
		t.Fatal("QR code not printed")
	}
	// Passkey progress items are informational; the session goes on
	fw.send(whatsmeow.QRChannelItem{Event: "passkey-challenge"})
	fw.send(codeItem("qr-2"))
	if got := <-printed; got != "qr-2" {
		t.Fatalf("printed %q", got)
	}
	st := pm.Status()
	if st.Pairing != pairQR || st.QR != "qr-2" {
		t.Fatalf("status = %+v", st)
	}
	if _, d, _ := fw.counts(); d != 0 {
		t.Fatalf("disconnects = %d", d)
	}
}

func TestOnLoggedOutIgnoresStaleClientAndLogsFactoryFailure(t *testing.T) {
	t.Run("stale client", func(t *testing.T) {
		fw, stale := newFakeWA(true), newFakeWA(true)
		pm := newTestPairing(fw, newFakeWA(false))
		pm.handleEvent(stale, &events.LoggedOut{})
		if pm.current() != waClient(fw) || !pm.Status().LoggedIn {
			t.Fatal("a logout of a replaced client must not touch the current one")
		}
		if _, d, _ := stale.counts(); d != 0 {
			t.Fatal("stale client must not be disconnected again")
		}
	})
	t.Run("factory fails", func(t *testing.T) {
		fw := newFakeWA(true)
		pm := newTestPairing(fw, nil)
		out := captureStderr(t, func() { pm.handleEvent(fw, &events.LoggedOut{}) })
		if pm.current() != waClient(fw) || pm.Status().LoggedIn {
			t.Fatal("client stays but is no longer logged in")
		}
		if !strings.Contains(out, "Failed to create a new client after logout") {
			t.Fatalf("log = %q", out)
		}
		waitFor(t, "old client disconnected", func() bool { _, d, _ := fw.counts(); return d == 1 })
	})
}

func TestReconnectConnectFailure(t *testing.T) {
	t.Run("after replacement the state is restored", func(t *testing.T) {
		fw := newFakeWA(true)
		pm := newTestPairing(fw, nil)
		pm.handleEvent(fw, &events.StreamReplaced{})
		fw.connectErr = errors.New("dial refused")
		err := pm.Reconnect()
		if err == nil || !strings.Contains(err.Error(), "reconnect: dial refused") {
			t.Fatalf("err = %v", err)
		}
		if st := pm.Status(); st.Pairing != pairReplaced {
			t.Fatalf("pairing = %q, want replaced", st.Pairing)
		}
	})
	t.Run("plain disconnect stays not replaced", func(t *testing.T) {
		fw := newFakeWA(true)
		fw.conn = false
		fw.connectErr = errors.New("dial refused")
		pm := newTestPairing(fw, nil)
		if err := pm.Reconnect(); err == nil {
			t.Fatal("expected an error")
		}
		if st := pm.Status(); st.Pairing != pairIdle {
			t.Fatalf("pairing = %q, want idle", st.Pairing)
		}
	})
}

func TestMapPairPhoneError(t *testing.T) {
	cases := []struct {
		err    error
		status int
		msg    string
	}{
		{whatsmeow.ErrPhoneNumberTooShort, http.StatusBadRequest, "phone number too short"},
		{whatsmeow.ErrPhoneNumberIsNotInternational, http.StatusBadRequest, "international phone number required"},
		{whatsmeow.ErrIQRateOverLimit, http.StatusTooManyRequests, "rate-limited"},
		{whatsmeow.ErrIQBadRequest, http.StatusBadRequest, "rejected the request"},
		{whatsmeow.ErrIQTimedOut, http.StatusGatewayTimeout, "timed out"},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, "timed out"},
		{whatsmeow.ErrNotConnected, http.StatusBadGateway, "connection to WhatsApp dropped"},
		{whatsmeow.ErrIQDisconnected, http.StatusBadGateway, "connection to WhatsApp dropped"},
		{errors.New("weird"), http.StatusBadGateway, "pairing failed: weird"},
	}
	for _, tc := range cases {
		got := mapPairPhoneError(tc.err)
		if got.status != tc.status || !strings.Contains(got.msg, tc.msg) {
			t.Errorf("%v: got %d %q, want %d containing %q", tc.err, got.status, got.msg, tc.status, tc.msg)
		}
	}
}

func TestPairPhoneCancelledWhileWaitingForFirstCode(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	pm.waitCode = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { _, err := pm.PairPhone(ctx, "+49 170 1234567"); res <- err }()
	waitFor(t, "session armed", func() bool { _, _, q := fw.counts(); return q == 1 && pm.Status().Pairing == pairQR })
	cancel()
	select {
	case err := <-res:
		var pe *pairError
		if !errors.As(err, &pe) || pe.status != http.StatusGatewayTimeout || pe.msg != "request cancelled" {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(settleTimeout):
		t.Fatal("PairPhone did not return")
	}
}

func TestPairPhoneSessionEndsBeforeReady(t *testing.T) {
	fw := newFakeWA(false)
	pm := newTestPairing(fw, nil)
	pm.waitCode = time.Hour
	res := make(chan error, 1)
	go func() { _, err := pm.PairPhone(context.Background(), "+49 170 1234567"); res <- err }()
	waitFor(t, "session armed", func() bool { _, _, q := fw.counts(); return q == 1 && pm.Status().Pairing == pairQR })
	// The session times out before any QR code arrived
	fw.send(whatsmeow.QRChannelTimeout)
	select {
	case err := <-res:
		var pe *pairError
		if !errors.As(err, &pe) || pe.status != http.StatusBadGateway || !strings.Contains(pe.msg, "ended before it was ready") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(settleTimeout):
		t.Fatal("PairPhone did not return")
	}
}

func TestPairingEndpointsRejectWrongMethods(t *testing.T) {
	pm := newTestPairing(newFakeWA(false), nil)
	h := newPairAPI(t, pm, "")
	for path, method := range map[string]string{
		"/api/health":        http.MethodPost,
		"/api/status":        http.MethodPost,
		"/api/start-pairing": http.MethodGet,
		"/api/reconnect":     http.MethodGet,
		"/api/pair-phone":    http.MethodGet,
	} {
		if rec := do(h, method, path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: code = %d", method, path, rec.Code)
		}
	}
}

func TestPairingEndpointsWithoutManagerAnswer503(t *testing.T) {
	_, h := newTestAPI(t, &fakeClient{}, "")
	for path, method := range map[string]string{
		"/api/status":        http.MethodGet,
		"/api/start-pairing": http.MethodPost,
		"/api/reconnect":     http.MethodPost,
		"/api/pair-phone":    http.MethodPost,
	} {
		rec := do(h, method, path, `{"phone":"+491701234567"}`)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "pairing is not available") {
			t.Errorf("%s: code=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestHealthReportsServiceAndAuth(t *testing.T) {
	_, h := newTestAPI(t, &fakeClient{}, "secret")
	rec := do(h, http.MethodGet, "/api/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	resp := decode[healthResponse](t, rec.Body.String())
	if resp.Service != "whatsapp-bridge" || resp.Version != bridgeVersion || resp.PID != os.Getpid() || !resp.AuthRequired || resp.StoreDir == "" {
		t.Fatalf("health = %+v", resp)
	}
}

func TestStartPairingAndReconnectEndpointErrors(t *testing.T) {
	fw := newFakeWA(false)
	fw.qrErr = errors.New("qr unavailable")
	h := newPairAPI(t, newTestPairing(fw, nil), "")
	rec := do(h, http.MethodPost, "/api/start-pairing", "{}")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "qr unavailable") {
		t.Fatalf("start-pairing: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Not logged in: reconnecting is a conflict
	rec = do(h, http.MethodPost, "/api/reconnect", "{}")
	if rec.Code != http.StatusConflict {
		t.Fatalf("reconnect: code=%d body=%s", rec.Code, rec.Body.String())
	}
}
