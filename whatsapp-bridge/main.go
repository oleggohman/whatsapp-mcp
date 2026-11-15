package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Handle regular incoming messages with media support
func handleMessage(r *chatResolver, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	ctx := context.Background()
	defer r.lockChat(msg.Info.Chat)()

	// Normalize LID addressing to phone numbers where a mapping is known
	chat := messageChatJID(ctx, r.lidStore(), &msg.Info)
	chatJID := chat.String()
	sender := messageSenderUser(ctx, r.lidStore(), &msg.Info)
	r.mergeIfMapped(ctx, msg.Info.Chat, chat)

	// Name hints only make sense for the other party of an incoming DM
	var hints nameHints
	if !msg.Info.IsFromMe && !msg.Info.IsGroup {
		hints.pushName = msg.Info.PushName
		if vn := msg.Info.VerifiedName; vn != nil && vn.Details != nil {
			hints.verifiedName = vn.Details.GetVerifiedName()
		}
	}
	name := r.chatName(ctx, chat, hints)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log metadata only, never message bodies or file names
		logf("[%s] %s %s in %s: %d text bytes, media %q\n", timestamp, direction, sender, chatJID, len(content), mediaType)
	}
}

// deviceContainer is the part of *sqlstore.Container that run needs.
type deviceContainer interface {
	GetFirstDevice(ctx context.Context) (*store.Device, error)
	NewDevice() *store.Device
	Close() error
}

// wmClient is what buildClient needs from a whatsmeow client beyond waClient.
type wmClient interface {
	waClient
	AddEventHandler(handler whatsmeow.EventHandler) uint32
	GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
}

// Seams for run: the defaults use the real whatsmeow client, device store and OS signals;
// tests swap them to run the bridge without contacting WhatsApp.
var (
	openDeviceContainer = func(ctx context.Context, dsn string, log waLog.Logger) (deviceContainer, error) {
		c, err := sqlstore.New(ctx, "sqlite3", dsn, log)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	newWMClient = func(dev *store.Device, log waLog.Logger) wmClient {
		c := whatsmeow.NewClient(dev, log)
		c.InitialAutoReconnect = true
		return realClient{c}
	}
	notifySignals = func(c chan<- os.Signal) { signal.Notify(c, syscall.SIGINT, syscall.SIGTERM) }
	isTTY         = stderrIsTTY
)

// Name and platform shown for this bridge in WhatsApp's "Linked devices" list.
// WhatsApp only accepts its fixed platform types, so the bridge reports itself as a desktop app.
const linkedDeviceName = "MCP-CC"

// setDeviceIdentity sets how the bridge appears in the phone's linked devices list.
// WhatsApp stores this at pairing time, so an already linked bridge keeps its old name
// until it is unlinked and paired again.
func setDeviceIdentity() {
	store.SetOSInfo(linkedDeviceName, [3]uint32{1, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
}

// run starts the bridge and returns the process exit code.
func run() int {
	setDeviceIdentity()
	// Headless daemon: all logs go to stderr
	logger := newStderrLogger("Client", "INFO")
	logger.Infof("Starting WhatsApp client...")
	dbLog := newStderrLogger("Database", "INFO")

	// Create directory for database if it doesn't exist
	dir := storeDir()
	if err := os.MkdirAll(dir, 0750); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return 1
	}

	// One bridge per store dir; a peer that holds the lock is running or starting
	lock, err := acquireLock(filepath.Join(dir, "bridge.lock"))
	if errors.Is(err, errLockHeld) {
		logger.Errorf("Another bridge is already running for store %s", dir)
		return exitPeerRunning
	}
	if err != nil {
		logger.Errorf("Failed to take the store lock: %v", err)
		return 1
	}
	defer lock.Release()

	token, err := loadToken(dir)
	if err != nil {
		logger.Errorf("Failed to load API token: %v", err)
		return 1
	}

	container, err := openDeviceContainer(context.Background(), sqliteDSN(filepath.Join(dir, "whatsapp.db")), dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return 1
	}

	// Get device store - This contains session information
	// Closed last (defers run in reverse), after the client disconnected; Windows cannot
	// delete an open database file.
	defer func() { _ = container.Close() }()
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return 1
		}
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return 1
	}
	defer func() { _ = messageStore.Close() }()

	var pm *pairingManager
	// The REST API exists before any client so each client can register how its sent messages are stored
	api := newAPIServer(func() bridgeClient { return pm.Client() }, messageStore, token, bridgeAddr())
	// buildClient creates a client for a device store and attaches all event handlers.
	// It is also used after a logout to build the replacement client.
	buildClient := func(dev *store.Device) waClient {
		c := newWMClient(dev, logger)
		resolver := newDeviceChatResolver(messageStore, dev,
			func(ctx context.Context, jid types.JID) (string, error) {
				info, err := c.GetGroupInfo(ctx, jid)
				if err != nil {
					return "", err
				}
				return info.Name, nil
			})
		api.sent.register(c, func(sent *sentMessage) {
			handleMessage(resolver, messageStore, sentEvent(selfJID(dev, sent), sent), logger)
		})
		c.AddEventHandler(func(evt interface{}) {
			// Pairing/login state machine first, then message processing
			pm.handleEvent(c, evt)
			switch v := evt.(type) {
			case *events.Message:
				handleMessage(resolver, messageStore, v, logger)
			case *events.HistorySync:
				handleHistorySync(resolver, selfUser(dev), messageStore, v, logger)
				resolver.trigger()
			case *events.Connected:
				logger.Infof("Connected to WhatsApp")
				resolver.trigger()
			case *events.LoggedOut:
				logger.Warnf("Device logged out; start pairing again to log in")
			}
		})
		return c
	}
	pm = newPairingManager(buildClient(deviceStore), func() (waClient, error) {
		return buildClient(container.NewDevice()), nil
	})
	if isTTY() {
		pm.printQR = func(code string) {
			out := os.Stderr
			_, _ = fmt.Fprintln(out, "\nScan this QR code with your WhatsApp app:")
			qrterminal.GenerateHalfBlock(code, qrterminal.L, out)
		}
	}

	// Start the REST API before connecting; handlers answer "Not connected" until the client is up.
	// The connecting routes answer 503 from the first request until the search migration is done.
	pm.migrating.Store(true)
	idle := newIdleTracker()
	api.pairing = pm
	api.idle = idle
	restServer, _, err := startRESTServer(bridgeAddr(), api.handler())
	if err != nil {
		logger.Errorf("Failed to start REST API server: %v", err)
		return 1
	}
	defer func() {
		if err := shutdownRESTServer(restServer, restShutdownTimeout); err != nil {
			logger.Warnf("REST API shutdown: %v (remaining connections closed)", err)
		}
	}()

	exitChan := make(chan os.Signal, 1)
	notifySignals(exitChan)

	// Build the search index while /api/health already answers. The routes that would
	// connect return 503 until it is done, so no client writes during the migration.
	migCtx, cancelMigration := context.WithCancel(context.Background())
	defer cancelMigration()
	migrationDone, watcherDone := make(chan struct{}), make(chan struct{})
	// The watcher cancels the migration on a stop request and hands the request on, so the
	// final select below still sees it. It does not touch the database.
	go func() {
		defer close(watcherDone)
		select {
		case sig := <-exitChan:
			cancelMigration()
			select {
			case exitChan <- sig:
			default:
			}
		case <-api.shutdown:
			cancelMigration()
			select {
			case api.shutdown <- struct{}{}:
			default:
			}
		case <-migrationDone:
		}
	}()
	migStart := time.Now()
	migErr := runMigrateSearch(messageStore, migCtx)
	close(migrationDone)
	<-watcherDone
	pm.migrating.Store(false)
	// Health polls do not count as activity, so a long migration must not use up the idle window
	idle.Touch()
	// The watcher has returned, so nothing cancels migCtx from here on
	cancelled := migCtx.Err() != nil
	if !cancelled {
		if c := messageStore.ftsCheck; c.err != nil {
			logger.Warnf("Search index failed its start-up check after %s, rebuilding it: %v", c.took.Round(time.Millisecond), c.err)
		} else if c.ran {
			logger.Infof("Search index start-up checks passed in %s", c.took.Round(time.Millisecond))
		}
		if migErr != nil {
			// Search stays on LIKE; the bridge still serves
			logger.Errorf("Search index migration failed after %s: %v", time.Since(migStart).Round(time.Millisecond), migErr)
			if errors.Is(migErr, errFTSNotDroppable) {
				logger.Errorf("The search index table messages_fts in %s is damaged and SQLite cannot drop it. "+
					"Messages are still stored and search still works through LIKE, only slower. This repeats on every "+
					"start until the table is removed by hand: stop the bridge and follow %q under Troubleshooting in README.md",
					messagesDBPath(), ftsRecoverySection)
			}
		} else {
			logger.Infof("Search index ready in %s", time.Since(migStart).Round(time.Millisecond))
		}
	}
	// A stop sent as the migration returned can lose the watcher's select to migrationDone.
	// It is still queued, so do not connect; the final select reads it.
	stopped := cancelled || stopPending(exitChan, api.shutdown)

	idleCtx, stopIdle := context.WithCancel(context.Background())
	defer stopIdle()
	idleExit := make(chan struct{})
	if !stopped {
		if c := pm.current(); c.Paired() {
			// Already logged in, just connect; failures stay visible in /api/status
			if err := c.Connect(); err != nil {
				logger.Errorf("Failed to connect: %v", err)
			}
		} else if isTTY() {
			// Interactive run: show a QR code right away. Headless runs wait for an explicit request.
			if _, err := pm.StartPairing(); err != nil {
				logger.Errorf("Failed to start pairing: %v", err)
			}
		}
		// watchIdle calls this at most once
		watchIdle(idleCtx, idle, idleExitDuration(), func() { close(idleExit) })
	}

	logln("REST server is running. Press Ctrl+C to disconnect and exit.")
	select {
	case <-exitChan:
	case <-api.shutdown:
		logger.Infof("Shutdown requested via API; exiting")
	case <-idleExit:
		logger.Infof("No API activity for %s; exiting", idleExitDuration())
	}

	logln("Disconnecting...")
	pm.current().Disconnect()
	return 0
}

// stopPending reports whether a signal or an API shutdown request is queued. It puts the value
// back (non-blocking, like the watcher) so the final select still reads it.
func stopPending(exitChan chan os.Signal, shutdown chan struct{}) bool {
	select {
	case sig := <-exitChan:
		select {
		case exitChan <- sig:
		default:
		}
	case <-shutdown:
		select {
		case shutdown <- struct{}{}:
		default:
		}
	default:
		return false
	}
	return true
}

// runMigrateSearch is a seam so tests can replace the search migration.
var runMigrateSearch = (*MessageStore).migrateSearch

// selfUser returns the bare user digits of the logged-in account ("" before pairing).
// selfJID is our own phone-number JID for storing a sent message; the send response's
// Sender is often our LID and is documented as unreliable, so it is only a fallback.
func selfJID(dev *store.Device, sent *sentMessage) types.JID {
	if dev.ID != nil {
		return dev.ID.ToNonAD()
	}
	return sent.resp.Sender
}

// sentEvent describes a message this device sent as the event whatsmeow emits for one sent
// from another device of the account, so it is stored exactly like those.
func sentEvent(self types.JID, sent *sentMessage) *events.Message {
	ts := sent.resp.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     sent.chat,
				Sender:   self,
				IsFromMe: true,
				IsGroup:  sent.chat.Server == types.GroupServer,
			},
			ID:        sent.resp.ID,
			Timestamp: ts,
		},
		Message: sent.msg,
	}
}

func selfUser(dev *store.Device) string {
	if dev.ID == nil {
		return ""
	}
	return dev.ID.User
}

// conversationName returns the best name carried by a history-sync conversation.
func conversationName(conv *waHistorySync.Conversation) string {
	if n := conv.GetName(); n != "" {
		return n
	}
	return conv.GetDisplayName()
}

// Handle history sync events
func handleHistorySync(r *chatResolver, self string, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	logf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	ctx := context.Background()
	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		syncedCount += syncConversation(ctx, r, self, messageStore, conversation, logger)
	}

	logf("History sync complete. Stored %d messages.\n", syncedCount)
}

// syncConversation stores one history-sync conversation and returns the stored message count.
// A non-group conversation holds the repair lock for its normalize + storeHistoryBatch
// writes only, so a repair cannot merge its LID chat in between (see lockChat) while live
// handlers are not stalled for the whole sync. Group names may need the network; groups are
// never merged, so they are resolved without the lock.
func syncConversation(ctx context.Context, r *chatResolver, self string, messageStore *MessageStore, conversation *waHistorySync.Conversation, logger waLog.Logger) int {
	syncedCount := 0
	// Parse JID from the conversation
	if conversation.ID == nil {
		return syncedCount
	}

	// Try to parse the JID
	origJID, err := types.ParseJID(*conversation.ID)
	if err != nil {
		logger.Warnf("Failed to parse JID %s: %v", *conversation.ID, err)
		return syncedCount
	}

	unlock := r.lockChat(origJID)
	defer unlock()

	// Normalize LID chats to their phone number; the conversation may carry the PN itself
	var altPN types.JID
	if pn := conversation.GetPnJID(); pn != "" {
		altPN, _ = types.ParseJID(pn)
	}
	jid := normalizeJID(ctx, r.lidStore(), origJID, altPN)
	chatJID := jid.String()
	r.mergeIfMapped(ctx, origJID, jid)
	if lidStr := conversation.GetLidJID(); lidStr != "" && jid.Server == types.DefaultUserServer {
		if lid, err := types.ParseJID(lidStr); err == nil {
			r.mergeIfMapped(ctx, lid, jid)
		}
	}

	// Get appropriate chat name from the stored name, contacts and the conversation itself
	name := r.chatName(ctx, jid, nameHints{convName: conversationName(conversation)})

	messages := conversation.Messages
	if len(messages) == 0 {
		return syncedCount
	}
	latestMsg := messages[0]
	if latestMsg == nil || latestMsg.Message == nil || latestMsg.Message.GetMessageTimestamp() == 0 {
		return syncedCount
	}

	// The chat time is the newest timestamp in the batch; the store keeps the newer of that
	// and the stored value, so an older chunk never moves the chat backward.
	latest := time.Time{}
	var batch []historyMessage
	for _, msg := range messages {
		if msg == nil || msg.Message == nil {
			continue
		}

		// Get message timestamp
		ts := msg.Message.GetMessageTimestamp()
		if ts == 0 {
			continue
		}
		timestamp := time.Unix(int64(ts), 0) //nolint:gosec // G115: seconds since epoch, far below MaxInt64
		if timestamp.After(latest) {
			latest = timestamp
		}

		// Extract text content
		var content string
		if msg.Message.Message != nil {
			if conv := msg.Message.Message.GetConversation(); conv != "" {
				content = conv
			} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
				content = ext.GetText()
			}
		}

		// Extract media info
		var mediaType, filename, url string
		var mediaKey, fileSHA256, fileEncSHA256 []byte
		var fileLength uint64

		if msg.Message.Message != nil {
			mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
		}

		// Skip messages with no content and no media
		if content == "" && mediaType == "" {
			continue
		}

		// Determine sender (always bare user digits)
		isFromMe := false
		participant := ""
		if msg.Message.Key != nil {
			if msg.Message.Key.FromMe != nil {
				isFromMe = *msg.Message.Key.FromMe
			}
			if msg.Message.Key.Participant != nil {
				participant = *msg.Message.Key.Participant
			}
		}
		sender := historySender(ctx, r.lidStore(), participant, isFromMe, jid, self)

		msgID := ""
		if msg.Message.Key != nil && msg.Message.Key.ID != nil {
			msgID = *msg.Message.Key.ID
		}

		// Log metadata only, never message bodies or file names
		logger.Debugf("Storing message: [%s] %s -> %s: %d text bytes, media %q",
			timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, len(content), mediaType)
		batch = append(batch, historyMessage{
			id: msgID, sender: sender, content: content, timestamp: timestamp, isFromMe: isFromMe,
			mediaType: mediaType, filename: filename, url: url,
			mediaKey: mediaKey, fileSHA256: fileSHA256, fileEncSHA256: fileEncSHA256, fileLength: fileLength,
		})
	}

	// One transaction per conversation: the repair lock is held for a single commit, and a
	// failed insert rolls the whole conversation back, chat upsert included.
	if err := messageStore.storeHistoryBatch(chatJID, name, latest, batch); err != nil {
		logger.Warnf("Failed to store history conversation %s: %v", chatJID, err)
		return syncedCount
	}
	syncedCount = len(batch)
	logger.Infof("Stored message batch: %d messages -> %s", syncedCount, chatJID)
	return syncedCount
}

// historyClient is the part of the WhatsApp client that requestHistorySync needs.
type historyClient interface {
	IsConnected() bool
	Paired() bool
	BuildHistorySyncRequest(lastKnownMessageInfo *types.MessageInfo, count int) *waE2E.Message
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// Request history sync from the server
func requestHistorySync(client historyClient) {
	if client == nil {
		logln("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		logln("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if !client.Paired() {
		logln("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		logln("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		logf("Failed to request history sync: %v\n", err)
	} else {
		logln("History sync requested. Waiting for server response...")
	}
}
