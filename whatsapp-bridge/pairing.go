package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

// bridgeVersion is reported by /api/health. The MCP supervisor overrides it at build time
// (-ldflags "-X main.bridgeVersion=<source hash>") to detect daemons running stale code.
var bridgeVersion = "0.2.0"

// Pairing states reported in /api/status ("replaced" is derived from the replaced flag).
const (
	pairIdle     = "idle"
	pairQR       = "qr"
	pairCode     = "code"
	pairReplaced = "replaced"
)

// defaultPairWait is how long pair-phone waits for the first QR event of a session.
const defaultPairWait = 20 * time.Second

var (
	errAlreadyPaired = &pairError{status: http.StatusConflict, msg: "already logged in; log out on the phone first to pair again"}
	errNoPairing     = &pairError{status: http.StatusServiceUnavailable, msg: "pairing is not available"}
)

// pairError is an error with the HTTP status the API should answer with.
type pairError struct {
	status int
	msg    string
}

func (e *pairError) Error() string { return e.msg }

// waClient is what the pairing manager needs from the WhatsApp client, so tests can fake it.
type waClient interface {
	bridgeClient
	Connect() error
	Disconnect()
	Paired() bool
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType, clientDisplayName string) (string, error)
}

// realClient adapts *whatsmeow.Client to waClient.
type realClient struct{ *whatsmeow.Client }

// Paired reports whether the device store holds a WhatsApp login.
func (c realClient) Paired() bool { return c.Store.ID != nil }

// pairSession is one QR/pairing-code attempt (GetQRChannel + Connect).
type pairSession struct {
	client    waClient
	cancel    context.CancelFunc
	firstCode chan struct{} // closed on the first QR code or when the session ends
	once      sync.Once
	done      chan struct{} // closed when the session ends, whatever the reason
	doneOnce  sync.Once
	paired    bool // pairing succeeded; guarded by pairingManager.mu
}

func (s *pairSession) signalFirst() { s.once.Do(func() { close(s.firstCode) }) }

// pairingManager owns the current client and the pairing state machine. It is fed by
// whatsmeow events (handleEvent) and QR channel items (handleQR) and never touches the network itself.
type pairingManager struct {
	mu        sync.Mutex
	client    waClient
	newClient func() (waClient, error) // builds a fresh unpaired client with handlers attached
	printQR   func(code string)        // optional, set only for interactive terminals
	waitCode  time.Duration

	loggedIn  bool
	replaced  bool
	qr        string
	qrExpires time.Time
	sess      *pairSession

	phoneCodeActive bool // a phone pairing code was issued for the current session

	startMu sync.Mutex // serialises session starts and reconnects
	phoneMu sync.Mutex // serialises pair-phone requests
}

// newPairingManager wraps the initial client; newClient builds a replacement after a logout.
func newPairingManager(c waClient, newClient func() (waClient, error)) *pairingManager {
	return &pairingManager{client: c, newClient: newClient, waitCode: defaultPairWait, loggedIn: c.Paired()}
}

// Client returns the current client (nil-safe interface for the REST layer).
func (pm *pairingManager) Client() bridgeClient {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.client
}

// current returns the current waClient.
func (pm *pairingManager) current() waClient {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.client
}

// statusResponse is the body of GET /api/status.
type statusResponse struct {
	Connected    bool   `json:"connected"`
	LoggedIn     bool   `json:"logged_in"`
	Pairing      string `json:"pairing"`
	QR           string `json:"qr,omitempty"`
	QRExpiresAt  int64  `json:"qr_expires_at,omitempty"` // unix seconds
	PairingError string `json:"pairing_error,omitempty"`
}

// Status snapshots the current state. IsConnected runs after pm.mu is released: it waits for
// whatsmeow's socket lock, which a Connect holds for the whole dial and handshake.
func (pm *pairingManager) Status() statusResponse {
	pm.mu.Lock()
	c := pm.client
	st := statusResponse{LoggedIn: pm.loggedIn, Pairing: pairIdle}
	switch {
	case pm.replaced:
		st.Pairing = pairReplaced
	case pm.sess != nil && pm.phoneCodeActive:
		st.Pairing = pairCode
	case pm.sess != nil:
		st.Pairing = pairQR
	}
	if pm.sess != nil && pm.qr != "" {
		st.QR = pm.qr
		st.QRExpiresAt = pm.qrExpires.Unix()
	}
	replaced := pm.replaced
	pm.mu.Unlock()
	st.Connected = !replaced && c.IsConnected()
	return st
}

// endSessionLocked tears down the session state; the caller holds pm.mu. It does not cancel the
// QR channel context: whatsmeow's QR emitter disconnects the client when that context ends before
// the channel saw PairSuccess, which would drop a freshly paired client. consumeQR cancels it once
// the channel has closed.
func (pm *pairingManager) endSessionLocked(s *pairSession) {
	if pm.sess != s || s == nil {
		return
	}
	pm.sess = nil
	pm.phoneCodeActive = false
	pm.qr = ""
	pm.qrExpires = time.Time{}
	s.signalFirst()
	s.doneOnce.Do(func() { close(s.done) })
}

// StartPairing arms a QR session if none is active. GetQRChannel must come before Connect.
func (pm *pairingManager) StartPairing() (*pairSession, error) {
	pm.startMu.Lock()
	defer pm.startMu.Unlock()

	pm.mu.Lock()
	c := pm.client
	if pm.loggedIn || c.Paired() {
		pm.mu.Unlock()
		return nil, errAlreadyPaired
	}
	if pm.sess != nil {
		s := pm.sess
		pm.mu.Unlock()
		return s, nil
	}
	pm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.GetQRChannel(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start pairing: %w", err)
	}
	if err := c.Connect(); err != nil {
		cancel()
		// GetQRChannel registered an event handler on c that cancel() does not remove. Left in
		// place, it would disconnect the client on the next attempt's first QR code, so the
		// next attempt gets a fresh client instead.
		pm.replaceUnpairedClient(c)
		return nil, fmt.Errorf("start pairing: connect: %w", err)
	}
	s := &pairSession{client: c, cancel: cancel, firstCode: make(chan struct{}), done: make(chan struct{})}
	pm.mu.Lock()
	if pm.client != c {
		// Logged out and swapped while connecting; drop this attempt.
		pm.mu.Unlock()
		cancel()
		c.Disconnect()
		return nil, errors.New("client was replaced while starting pairing")
	}
	pm.sess = s
	pm.replaced = false
	pm.mu.Unlock()
	go pm.consumeQR(s, ch)
	return s, nil
}

// replaceUnpairedClient swaps in a fresh client for c (still current, unpaired); on failure c stays.
func (pm *pairingManager) replaceUnpairedClient(c waClient) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.client != c || pm.newClient == nil || c.Paired() {
		return
	}
	nc, err := pm.newClient()
	if err != nil {
		logf("Failed to create a new client after a failed pairing connect: %v\n", err)
		return
	}
	pm.client = nc
}

// consumeQR feeds QR channel items into the state machine until the session ends. whatsmeow
// does not close the channel after every failure (for example a passkey error followed by our
// Disconnect), so a session that ended without pairing is released here instead of waiting
// for a close that never comes. After a successful pairing the context is still cancelled
// only once the channel has closed (see endSessionLocked).
func (pm *pairingManager) consumeQR(s *pairSession, ch <-chan whatsmeow.QRChannelItem) {
	for open := true; open; {
		select {
		case item, ok := <-ch:
			if !ok {
				open = false
				break
			}
			pm.handleQR(s, item)
		case <-s.done:
			pm.mu.Lock()
			paired := s.paired
			pm.mu.Unlock()
			if !paired {
				s.cancel()
				return
			}
			// Paired: wait for whatsmeow to close the channel before cancelling its context.
			for item := range ch {
				pm.handleQR(s, item)
			}
			open = false
		}
	}
	// Channel closed: whatever the reason, the session is over.
	pm.mu.Lock()
	pm.endSessionLocked(s)
	pm.mu.Unlock()
	s.cancel()
}

// handleQR applies one QR channel item.
func (pm *pairingManager) handleQR(s *pairSession, item whatsmeow.QRChannelItem) {
	pm.mu.Lock()
	if pm.sess != s {
		pm.mu.Unlock()
		return
	}
	switch {
	case item.Event == whatsmeow.QRChannelEventCode:
		pm.qr = item.Code
		pm.qrExpires = time.Now().Add(item.Timeout)
		s.signalFirst()
		print := pm.printQR
		pm.mu.Unlock()
		if print != nil {
			print(item.Code)
		}
	case item.Event == whatsmeow.QRChannelSuccess.Event:
		// Do not wait for the channel to close: the account is usable right now.
		pm.loggedIn = true
		pm.replaced = false
		s.paired = true
		pm.endSessionLocked(s)
		pm.mu.Unlock()
	case strings.HasPrefix(item.Event, "passkey"):
		pm.mu.Unlock()
	default:
		// timeout, error, err-*: stop trying and go idle until asked again (no process exit).
		// Tear down under startMu (lock order startMu, then mu) so a StartPairing on the same
		// client cannot connect a new session between the teardown and the Disconnect below.
		pm.mu.Unlock()
		pm.startMu.Lock()
		defer pm.startMu.Unlock()
		pm.mu.Lock()
		if pm.sess != s {
			// Already ended elsewhere (success, logout or a newer session): nothing to tear down.
			pm.mu.Unlock()
			return
		}
		pm.endSessionLocked(s)
		pm.mu.Unlock()
		if item.Error != nil {
			logf("Pairing ended: %s: %v\n", item.Event, item.Error)
		} else {
			logf("Pairing ended: %s\n", item.Event)
		}
		s.client.Disconnect()
	}
}

// handleEvent applies a whatsmeow event from src; events of a replaced client are ignored.
func (pm *pairingManager) handleEvent(src waClient, evt any) {
	switch evt.(type) {
	case *events.PairSuccess:
		pm.mu.Lock()
		if pm.client == src {
			pm.loggedIn = true
			pm.replaced = false
			if pm.sess != nil {
				pm.sess.paired = true
			}
			pm.endSessionLocked(pm.sess)
		}
		pm.mu.Unlock()
	case *events.Connected:
		pm.mu.Lock()
		if pm.client == src {
			pm.loggedIn = true
			pm.replaced = false
		}
		pm.mu.Unlock()
	case *events.StreamReplaced:
		// Another process took over this login. Reconnecting here would start a kick war.
		pm.mu.Lock()
		if pm.client == src {
			pm.replaced = true
		}
		pm.mu.Unlock()
		logln("Stream replaced by another client; not reconnecting. Use POST /api/reconnect to take it back.")
	case *events.LoggedOut:
		pm.onLoggedOut(src)
	}
}

// onLoggedOut swaps in a fresh unpaired client; whatsmeow already deleted the old device.
func (pm *pairingManager) onLoggedOut(src waClient) {
	pm.mu.Lock()
	if pm.client != src {
		pm.mu.Unlock()
		return
	}
	logln("Device logged out; ready for a new pairing")
	pm.loggedIn = false
	pm.replaced = false
	pm.endSessionLocked(pm.sess)
	if pm.newClient != nil {
		nc, err := pm.newClient()
		if err != nil {
			logf("Failed to create a new client after logout: %v\n", err)
		} else {
			pm.client = nc
		}
	}
	pm.mu.Unlock()
	// Not inside the event handler's goroutine, to avoid re-entering whatsmeow locks.
	go src.Disconnect()
}

// Reconnect explicitly reconnects a paired client (the only way out of "replaced").
func (pm *pairingManager) Reconnect() error {
	pm.startMu.Lock()
	defer pm.startMu.Unlock()
	pm.mu.Lock()
	c := pm.client
	wasReplaced := pm.replaced
	pm.mu.Unlock()
	if !c.Paired() {
		return &pairError{status: http.StatusConflict, msg: "not logged in; start pairing instead"}
	}
	// Outside pm.mu: IsConnected waits for whatsmeow's socket lock (see Status).
	if !wasReplaced && c.IsConnected() {
		return nil
	}
	pm.mu.Lock()
	pm.replaced = false
	pm.mu.Unlock()
	if err := c.Connect(); err != nil {
		if wasReplaced {
			pm.mu.Lock()
			pm.replaced = true
			pm.mu.Unlock()
		}
		return fmt.Errorf("reconnect: %w", err)
	}
	return nil
}

// normalizePhone strips non-digits and enforces an international number.
func normalizePhone(raw string) (string, error) {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	p := b.String()
	switch {
	case len(p) <= 6:
		return "", &pairError{status: http.StatusBadRequest, msg: "phone number too short"}
	case len(p) > 15:
		return "", &pairError{status: http.StatusBadRequest, msg: "phone number too long (max 15 digits)"}
	case strings.HasPrefix(p, "0"):
		return "", &pairError{status: http.StatusBadRequest, msg: "international phone number required (country code, no leading 0)"}
	}
	return p, nil
}

// mapPairPhoneError turns a whatsmeow error into a clear API error.
func mapPairPhoneError(err error) *pairError {
	switch {
	case errors.Is(err, whatsmeow.ErrPhoneNumberTooShort):
		return &pairError{http.StatusBadRequest, "phone number too short"}
	case errors.Is(err, whatsmeow.ErrPhoneNumberIsNotInternational):
		return &pairError{http.StatusBadRequest, "international phone number required (country code, no leading 0)"}
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		return &pairError{http.StatusTooManyRequests, "WhatsApp rate-limited pairing requests; wait a few minutes and retry"}
	case errors.Is(err, whatsmeow.ErrIQBadRequest):
		return &pairError{http.StatusBadRequest, "WhatsApp rejected the request (check the phone number)"}
	case errors.Is(err, whatsmeow.ErrIQTimedOut), errors.Is(err, context.DeadlineExceeded):
		return &pairError{http.StatusGatewayTimeout, "timed out waiting for WhatsApp; retry"}
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrIQDisconnected):
		return &pairError{http.StatusBadGateway, "connection to WhatsApp dropped; retry"}
	default:
		return &pairError{http.StatusBadGateway, fmt.Sprintf("pairing failed: %v", err)}
	}
}

// PairPhone starts a session if needed, waits for the first QR event and requests a phone pairing code.
func (pm *pairingManager) PairPhone(ctx context.Context, raw string) (string, error) {
	phone, err := normalizePhone(raw)
	if err != nil {
		return "", err
	}
	pm.phoneMu.Lock()
	defer pm.phoneMu.Unlock()

	s, err := pm.StartPairing()
	if err != nil {
		var pe *pairError
		if errors.As(err, &pe) {
			return "", err
		}
		return "", &pairError{status: http.StatusBadGateway, msg: err.Error()}
	}
	select {
	case <-s.firstCode:
	case <-time.After(pm.waitCode):
		return "", &pairError{http.StatusGatewayTimeout, "timed out waiting for the pairing session to become ready"}
	case <-ctx.Done():
		return "", &pairError{http.StatusGatewayTimeout, "request cancelled"}
	}
	pm.mu.Lock()
	active := pm.sess == s
	pm.mu.Unlock()
	if !active {
		return "", &pairError{http.StatusBadGateway, "pairing session ended before it was ready; retry"}
	}
	code, err := s.client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return "", mapPairPhoneError(err)
	}
	pm.mu.Lock()
	if pm.sess == s {
		pm.phoneCodeActive = true
	}
	pm.mu.Unlock()
	return code, nil
}

// stderrIsTTY reports whether stderr is an interactive terminal.
func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// healthResponse is the body of GET /api/health.
type healthResponse struct {
	Service      string `json:"service"`
	Version      string `json:"version"`
	StoreDir     string `json:"store_dir"`
	PID          int    `json:"pid"`
	AuthRequired bool   `json:"auth_required"`
}

// handleHealth handles GET /api/health (open: no token, never resets the idle timer).
func (a *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Service:      "whatsapp-bridge",
		Version:      bridgeVersion,
		StoreDir:     a.storeDirAbs,
		PID:          os.Getpid(),
		AuthRequired: a.token != "",
	})
}

// writePairError answers with the status carried by err (500 when it has none).
func writePairError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var pe *pairError
	if errors.As(err, &pe) {
		status = pe.status
	}
	writeJSON(w, status, pairPhoneResponse{Success: false, Message: err.Error()})
}

// handleStatus handles GET /api/status. It is read-only: state changes need a POST, which a
// browser cannot send cross-site without an Origin header (rejected by the guard).
func (a *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.pairing == nil {
		writePairError(w, errNoPairing)
		return
	}
	q := r.URL.Query()
	if q.Has("reconnect") || q.Has("start_pairing") {
		writePairError(w, &pairError{status: http.StatusBadRequest,
			msg: "GET /api/status is read-only; use POST /api/reconnect or POST /api/start-pairing"})
		return
	}
	writeJSON(w, http.StatusOK, a.pairing.Status())
}

// handleStartPairing handles POST /api/start-pairing: arms a QR session if none is active
// and answers with the status. Being logged in already is not an error.
func (a *apiServer) handleStartPairing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.pairing == nil {
		writePairError(w, errNoPairing)
		return
	}
	if _, err := a.pairing.StartPairing(); err != nil && !errors.Is(err, errAlreadyPaired) {
		writePairError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.pairing.Status())
}

// handleReconnect handles POST /api/reconnect: reconnects a paired client (the way out of
// "replaced") and answers with the status.
func (a *apiServer) handleReconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.pairing == nil {
		writePairError(w, errNoPairing)
		return
	}
	if err := a.pairing.Reconnect(); err != nil {
		writePairError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.pairing.Status())
}

// pairPhoneRequest is the body of POST /api/pair-phone.
type pairPhoneRequest struct {
	Phone string `json:"phone"`
}

// pairPhoneResponse is the body of POST /api/pair-phone (also used for pairing errors).
type pairPhoneResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Code    string `json:"code,omitempty"`
}

// handlePairPhone handles POST /api/pair-phone.
func (a *apiServer) handlePairPhone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req pairPhoneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}
	if a.pairing == nil {
		writePairError(w, errNoPairing)
		return
	}
	code, err := a.pairing.PairPhone(r.Context(), req.Phone)
	if err != nil {
		writePairError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pairPhoneResponse{Success: true, Code: code, Message: "Enter this code in WhatsApp: Linked devices > Link with phone number"})
}
