package main

import (
	"context"
	"database/sql"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// fakeMeow is a wmClient: a fakeWA that records the event handler run installs.
type fakeMeow struct {
	*fakeWA
	hmu     sync.Mutex
	handler whatsmeow.EventHandler
	group   func(jid types.JID) (*types.GroupInfo, error)
}

func (f *fakeMeow) AddEventHandler(h whatsmeow.EventHandler) uint32 {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	f.handler = h
	return 1
}

func (f *fakeMeow) GetGroupInfo(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
	return f.group(jid)
}

// fire delivers evt to the handler like whatsmeow's event dispatcher.
func (f *fakeMeow) fire(evt any) {
	f.hmu.Lock()
	h := f.handler
	f.hmu.Unlock()
	h(evt)
}

// fakeContainer is a deviceContainer that serves a prepared device.
type fakeContainer struct {
	dev      *store.Device
	firstErr error
}

func (c *fakeContainer) GetFirstDevice(context.Context) (*store.Device, error) {
	return c.dev, c.firstErr
}
func (c *fakeContainer) NewDevice() *store.Device { return &store.Device{} }

// runHarness wires run() to fakes: temp store, random loopback port, captured logs, fake
// WhatsApp clients and a controllable signal channel. Nothing contacts WhatsApp.
type runHarness struct {
	t       *testing.T
	dir     string
	logs    *liveStderr
	restore func()
	sig     chan chan<- os.Signal

	// The real implementations the harness replaced, for tests that want them back
	realContainer func(context.Context, string, waLog.Logger) (deviceContainer, error)
	realClient    func(*store.Device, waLog.Logger) wmClient
	realSignals   func(chan<- os.Signal)

	mu      sync.Mutex
	clients []*fakeMeow
	// configure runs on every client run creates, with its creation index
	configure func(i int, c *fakeMeow)
}

var serverAddrRe = regexp.MustCompile(`REST API server on (127\.0\.0\.1:\d+)`)

// newRunHarness fakes the client factory and the device container (container may be nil
// to keep the real sqlstore on a temp file).
func newRunHarness(t *testing.T, container deviceContainer) *runHarness {
	t.Helper()
	h := &runHarness{t: t, dir: t.TempDir(), sig: make(chan chan<- os.Signal, 1)}
	t.Setenv("WHATSAPP_STORE_DIR", h.dir)
	t.Setenv("WHATSAPP_BRIDGE_ADDR", "127.0.0.1:0")
	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
	t.Setenv("WHATSAPP_BRIDGE_IDLE_EXIT", "")
	h.logs, h.restore = redirectStderr(t)

	oldContainer, oldClient, oldSignals, oldTTY := openDeviceContainer, newWMClient, notifySignals, isTTY
	h.realContainer, h.realClient, h.realSignals = oldContainer, oldClient, oldSignals
	t.Cleanup(func() {
		openDeviceContainer, newWMClient, notifySignals, isTTY = oldContainer, oldClient, oldSignals, oldTTY
	})
	if container != nil {
		openDeviceContainer = func(context.Context, string, waLog.Logger) (deviceContainer, error) { return container, nil }
	}
	newWMClient = func(*store.Device, waLog.Logger) wmClient {
		h.mu.Lock()
		defer h.mu.Unlock()
		c := &fakeMeow{fakeWA: newFakeWA(false), group: func(types.JID) (*types.GroupInfo, error) { return nil, io.ErrUnexpectedEOF }}
		if h.configure != nil {
			h.configure(len(h.clients), c)
		}
		h.clients = append(h.clients, c)
		return c
	}
	notifySignals = func(c chan<- os.Signal) { h.sig <- c }
	isTTY = func() bool { return false }
	return h
}

func (h *runHarness) client(i int) *fakeMeow {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients[i]
}

func (h *runHarness) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// start runs run() in the background and returns the channel carrying its exit code.
func (h *runHarness) start() <-chan int {
	code := make(chan int, 1)
	go func() { code <- run() }()
	return code
}

// addr waits for the REST server to come up and returns its address.
func (h *runHarness) addr() string {
	h.t.Helper()
	var m []string
	waitFor(h.t, "REST server address", func() bool {
		m = serverAddrRe.FindStringSubmatch(h.logs.String())
		return m != nil
	})
	return m[1]
}

// stop sends SIGTERM through the fake signal hook and returns run's exit code.
func (h *runHarness) stop(code <-chan int) int {
	h.t.Helper()
	select {
	case c := <-h.sig:
		c <- syscall.SIGTERM
	case <-time.After(settleTimeout):
		h.t.Fatal("run never registered for signals")
	}
	return h.wait(code)
}

func (h *runHarness) wait(code <-chan int) int {
	h.t.Helper()
	select {
	case c := <-code:
		return c
	case <-time.After(settleTimeout):
		h.t.Fatal("run did not return")
		return -1
	}
}

// registered waits until run reached its main loop (signals registered).
func (h *runHarness) registered() chan<- os.Signal {
	h.t.Helper()
	select {
	case c := <-h.sig:
		h.sig <- c // keep it for stop
		return c
	case <-time.After(settleTimeout):
		h.t.Fatal("run never registered for signals")
		return nil
	}
}

// chatNameInDB reads a chat name straight from messages.db.
func (h *runHarness) chatNameInDB(jid types.JID) (string, bool) {
	h.t.Helper()
	db, err := openMessagesDB(messagesDBPath())
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	var name sql.NullString
	if err := db.QueryRow("SELECT name FROM chats WHERE jid = ?", jid.String()).Scan(&name); err != nil {
		return "", false
	}
	return name.String, true
}

func (h *runHarness) get(addr, path string) (int, string) {
	h.t.Helper()
	cl := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: settleTimeout}
	resp, err := cl.Get("http://" + addr + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// seedStore creates messages.db in the harness store with a placeholder-named chat.
func (h *runHarness) seedStore(jid types.JID, name string) {
	h.t.Helper()
	ms, err := NewMessageStore()
	if err != nil {
		h.t.Fatal(err)
	}
	defer ms.Close()
	if err := ms.StoreChat(jid.String(), name, time.Now()); err != nil {
		h.t.Fatal(err)
	}
}

func TestRunFailsWhenStoreDirCannotBeCreated(t *testing.T) {
	h := newRunHarness(t, nil)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHATSAPP_STORE_DIR", filepath.Join(blocker, "store"))
	if code := run(); code != 1 {
		t.Fatalf("code = %d", code)
	}
	waitForText(t, h.logs, "Failed to create store directory")
}

func TestRunExitsWithTempFailWhenPeerHoldsLock(t *testing.T) {
	h := newRunHarness(t, nil)
	peer, err := acquireLock(filepath.Join(h.dir, "bridge.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Release()
	if code := run(); code != exitPeerRunning {
		t.Fatalf("code = %d, want %d", code, exitPeerRunning)
	}
	waitForText(t, h.logs, "Another bridge is already running")
}

func TestRunFailsOnUnusableLockFile(t *testing.T) {
	h := newRunHarness(t, nil)
	if err := os.Mkdir(filepath.Join(h.dir, "bridge.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := run(); code != 1 {
		t.Fatalf("code = %d", code)
	}
	waitForText(t, h.logs, "Failed to take the store lock")
}

func TestRunFailsOnEmptyTokenFile(t *testing.T) {
	h := newRunHarness(t, nil)
	if err := os.WriteFile(filepath.Join(h.dir, "bridge.token"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(); code != 1 {
		t.Fatalf("code = %d", code)
	}
	waitForText(t, h.logs, "Failed to load API token")
	if strings.Contains(h.logs.String(), "REST API server on") {
		t.Fatal("the server must not start without a usable token")
	}
}

func TestRunFailsWhenDatabaseCannotBeOpened(t *testing.T) {
	h := newRunHarness(t, nil)
	openDeviceContainer = func(context.Context, string, waLog.Logger) (deviceContainer, error) {
		return nil, io.ErrClosedPipe
	}
	if code := run(); code != 1 {
		t.Fatalf("code = %d", code)
	}
	waitForText(t, h.logs, "Failed to connect to database")
}

func TestRunFailsWhenDeviceCannotBeRead(t *testing.T) {
	h := newRunHarness(t, &fakeContainer{firstErr: io.ErrUnexpectedEOF})
	if code := run(); code != 1 {
		t.Fatalf("code = %d", code)
	}
	waitForText(t, h.logs, "Failed to get device")
}

func TestRunFailsWhenMessageStoreCannotBeOpened(t *testing.T) {
	h := newRunHarness(t, &fakeContainer{dev: &store.Device{}})
	if err := os.Mkdir(filepath.Join(h.dir, "messages.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := run(); code != 1 {
		t.Fatalf("code = %d", code)
	}
	waitForText(t, h.logs, "Failed to initialize message store")
}

func TestRunFailsWhenRESTServerCannotBind(t *testing.T) {
	h := newRunHarness(t, &fakeContainer{dev: &store.Device{}})
	t.Setenv("WHATSAPP_BRIDGE_ADDR", "not-an-address")
	if code := run(); code != 1 {
		t.Fatalf("code = %d", code)
	}
	waitForText(t, h.logs, "Failed to start REST API server")
}

// With no stored device, run creates one, an unpaired bridge only serves the REST API, and
// the idle timer ends the process without any signal.
func TestRunCreatesDeviceAndExitsWhenIdle(t *testing.T) {
	h := newRunHarness(t, &fakeContainer{firstErr: sql.ErrNoRows})
	t.Setenv("WHATSAPP_BRIDGE_IDLE_EXIT", "20ms")
	if code := h.wait(h.start()); code != 0 {
		t.Fatalf("code = %d", code)
	}
	logs := h.logs.String()
	for _, want := range []string{"Created new device", "No API activity for 20ms; exiting", "Disconnecting..."} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in:\n%s", want, logs)
		}
	}
	c := h.client(0)
	if connects, disconnects, qr := c.counts(); connects != 0 || qr != 0 || disconnects != 1 {
		t.Fatalf("unpaired headless client: connects=%d qr=%d disconnects=%d", connects, qr, disconnects)
	}
}

// The real sqlstore and whatsmeow client work offline: an unpaired device never dials out.
func TestRunWithRealStoreAndClientExitsWhenIdle(t *testing.T) {
	h := newRunHarness(t, nil)
	openDeviceContainer, newWMClient = h.realContainer, h.realClient
	t.Setenv("WHATSAPP_BRIDGE_IDLE_EXIT", "20ms")
	if code := h.wait(h.start()); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "whatsapp.db")); err != nil {
		t.Fatalf("device database not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "messages.db")); err != nil {
		t.Fatalf("message database not created: %v", err)
	}
}

func TestRunConnectsPairedClientAndLogsConnectFailure(t *testing.T) {
	for name, connectErr := range map[string]error{"ok": nil, "fails": io.ErrClosedPipe} {
		t.Run(name, func(t *testing.T) {
			h := newRunHarness(t, &fakeContainer{dev: &store.Device{}})
			h.configure = func(i int, c *fakeMeow) {
				c.paired, c.conn, c.connectErr = true, false, connectErr
			}
			code := h.start()
			h.registered()
			if connects, _, _ := h.client(0).counts(); connects != 1 {
				t.Fatalf("connects = %d", connects)
			}
			if connectErr != nil {
				waitForText(t, h.logs, "Failed to connect:")
			}
			if c := h.stop(code); c != 0 {
				t.Fatalf("code = %d", c)
			}
			h.restore() // flush the captured log
			if failed := strings.Contains(h.logs.String(), "Failed to connect:"); failed != (connectErr != nil) {
				t.Fatalf("connect failure logged = %v\n%s", failed, h.logs.String())
			}
			if _, d, _ := h.client(0).counts(); d != 1 {
				t.Fatalf("disconnects = %d", d)
			}
		})
	}
}

func TestRunInteractiveStartsPairingAndPrintsQR(t *testing.T) {
	t.Run("QR is shown", func(t *testing.T) {
		h := newRunHarness(t, &fakeContainer{dev: &store.Device{}})
		isTTY = func() bool { return true }
		h.configure = func(i int, c *fakeMeow) { c.autoCode = true }
		code := h.start()
		waitForText(t, h.logs, "Scan this QR code with your WhatsApp app")
		if c := h.stop(code); c != 0 {
			t.Fatalf("code = %d", c)
		}
		if connects, _, qr := h.client(0).counts(); connects != 1 || qr != 1 {
			t.Fatalf("connects=%d qr=%d", connects, qr)
		}
	})
	t.Run("pairing cannot start", func(t *testing.T) {
		h := newRunHarness(t, &fakeContainer{dev: &store.Device{}})
		isTTY = func() bool { return true }
		h.configure = func(i int, c *fakeMeow) { c.qrErr = io.ErrClosedPipe }
		code := h.start()
		waitForText(t, h.logs, "Failed to start pairing")
		if c := h.stop(code); c != 0 {
			t.Fatalf("code = %d", c)
		}
	})
}

// The event handler routes whatsmeow events into the stores and the pairing manager.
func TestRunEventHandlerStoresMessagesAndNamesGroups(t *testing.T) {
	dev := &store.Device{
		Contacts: &fakeContacts{m: map[string]types.ContactInfo{testPN: {FullName: "Alice"}}},
		LIDs:     newFakeLIDs(),
	}
	h := newRunHarness(t, &fakeContainer{dev: dev})
	h.configure = func(i int, c *fakeMeow) {
		c.group = func(jid types.JID) (*types.GroupInfo, error) {
			if jid.User == "120363000000000001" {
				return &types.GroupInfo{GroupName: types.GroupName{Name: "Fake Group"}}, nil
			}
			return nil, io.ErrUnexpectedEOF
		}
	}
	// A placeholder name the history-sync repair pass can upgrade through the contact store
	h.seedStore(pnJID(testPN), testPN)

	code := h.start()
	h.registered()
	c := h.client(0)

	send := func(chat types.JID, id string) {
		c.fire(&events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: chat, Sender: pnJID(testPN2), IsGroup: chat.Server == types.GroupServer},
				ID:            types.MessageID(id),
				Timestamp:     time.Now(),
				PushName:      "Pat",
			},
			Message: &waE2E.Message{Conversation: proto.String("hi")},
		})
	}
	named, failing := types.NewJID("120363000000000001", types.GroupServer), types.NewJID("120363000000000002", types.GroupServer)
	send(pnJID(testPN2), "dm1")
	send(named, "g1")
	send(failing, "g2")
	for jid, want := range map[types.JID]string{
		pnJID(testPN2): "Pat",
		named:          "Fake Group",
		failing:        "Group 120363000000000002",
	} {
		if got, ok := h.chatNameInDB(jid); !ok || got != want {
			t.Errorf("chat %s = %q (stored %v), want %q", jid, got, ok, want)
		}
	}

	// Unrelated events pass through untouched
	c.fire(&events.StreamReplaced{})

	// A history sync stores its messages and schedules one repair pass that renames the placeholder chat
	histChat := pnJID("15550100003")
	c.fire(&events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID: proto.String(histChat.String()),
		Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(histChat.String()), FromMe: proto.Bool(true), ID: proto.String("h1")},
			Message:          &waE2E.Message{Conversation: proto.String("hello")},
			MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
		}}},
	}}}})
	waitFor(t, "repair to rename the placeholder chat", func() bool {
		name, _ := h.chatNameInDB(pnJID(testPN))
		return name == "Alice"
	})

	status, body := h.get(h.addr(), "/api/health")
	if status != http.StatusOK || !strings.Contains(body, `"service":"whatsapp-bridge"`) {
		t.Fatalf("health = %d %s", status, body)
	}

	if code := h.stop(code); code != 0 {
		t.Fatalf("code = %d", code)
	}
}

func TestRunConnectedEventTriggersRepair(t *testing.T) {
	dev := &store.Device{
		Contacts: &fakeContacts{m: map[string]types.ContactInfo{testPN: {FullName: "Alice"}}},
		LIDs:     newFakeLIDs(),
	}
	h := newRunHarness(t, &fakeContainer{dev: dev})
	h.seedStore(pnJID(testPN), testPN)
	code := h.start()
	h.registered()
	h.client(0).fire(&events.Connected{})
	waitFor(t, "repair to rename the placeholder chat", func() bool {
		name, _ := h.chatNameInDB(pnJID(testPN))
		return name == "Alice"
	})
	waitForText(t, h.logs, "Connected to WhatsApp")
	if c := h.stop(code); c != 0 {
		t.Fatalf("code = %d", c)
	}
}

func TestRunLogoutBuildsReplacementClient(t *testing.T) {
	h := newRunHarness(t, &fakeContainer{dev: &store.Device{}})
	h.configure = func(i int, c *fakeMeow) {
		if i == 0 {
			c.paired, c.conn = true, true
		}
	}
	code := h.start()
	h.registered()
	h.client(0).fire(&events.LoggedOut{})
	waitForText(t, h.logs, "Device logged out; start pairing again to log in")
	if n := h.clientCount(); n != 2 {
		t.Fatalf("clients = %d, want the replacement to exist", n)
	}
	status, body := h.get(h.addr(), "/api/status")
	if status != http.StatusOK || !strings.Contains(body, `"logged_in":false`) {
		t.Fatalf("status = %d %s", status, body)
	}
	if c := h.stop(code); c != 0 {
		t.Fatalf("code = %d", c)
	}
	// The replacement is the client that gets disconnected at exit
	if _, d, _ := h.client(1).counts(); d != 1 {
		t.Fatalf("replacement disconnects = %d", d)
	}
}

// A connection that never finishes a request must not outlive the bridge.
func TestRunClosesStuckConnectionsOnShutdown(t *testing.T) {
	h := newRunHarness(t, &fakeContainer{dev: &store.Device{}})
	old := restShutdownTimeout
	restShutdownTimeout = 30 * time.Millisecond
	t.Cleanup(func() { restShutdownTimeout = old })
	code := h.start()
	h.registered()
	conn, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /api/health HTTP/1.1\r\nHost: localhost\r\n")); err != nil { // headers never end
		t.Fatal(err)
	}
	// The server accepts connections in order, so once this request is answered it has also
	// accepted (and tracks) the stuck connection above.
	if status, _ := h.get(h.addr(), "/api/health"); status != http.StatusOK {
		t.Fatalf("health status = %d", status)
	}
	if c := h.stop(code); c != 0 {
		t.Fatalf("code = %d", c)
	}
	waitForText(t, h.logs, "REST API shutdown:")
	_ = conn.SetReadDeadline(time.Now().Add(settleTimeout))
	if _, err := io.Copy(io.Discard, conn); err != nil && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("connection must be closed by the server: %v", err)
	}
}

func TestRealContainerFailsOnUnusableDatabase(t *testing.T) {
	h := newRunHarness(t, nil)
	// A directory cannot be opened as a SQLite database file
	c, err := h.realContainer(context.Background(), sqliteDSN(t.TempDir()), waLog.Noop)
	if err == nil || c != nil {
		t.Fatalf("container = %v, err = %v", c, err)
	}
}

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
