package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"go.mau.fi/whatsmeow"
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

func main() {
	os.Exit(run())
}

// run starts the bridge and returns the process exit code.
func run() int {
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

	container, err := sqlstore.New(context.Background(), "sqlite3", sqliteDSN(filepath.Join(dir, "whatsapp.db")), dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return 1
	}

	// Get device store - This contains session information
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
	defer messageStore.Close()

	var pm *pairingManager
	// buildClient creates a client for a device store and attaches all event handlers.
	// It is also used after a logout to build the replacement client.
	buildClient := func(dev *store.Device) waClient {
		c := whatsmeow.NewClient(dev, logger)
		c.InitialAutoReconnect = true
		rc := realClient{c}
		resolver := newDeviceChatResolver(messageStore, dev,
			func(ctx context.Context, jid types.JID) (string, error) {
				info, err := c.GetGroupInfo(ctx, jid)
				if err != nil {
					return "", err
				}
				return info.Name, nil
			})
		c.AddEventHandler(func(evt interface{}) {
			// Pairing/login state machine first, then message processing
			pm.handleEvent(rc, evt)
			switch v := evt.(type) {
			case *events.Message:
				handleMessage(resolver, messageStore, v, logger)
			case *events.HistorySync:
				handleHistorySync(resolver, selfUser(c), messageStore, v, logger)
				resolver.trigger()
			case *events.Connected:
				logger.Infof("Connected to WhatsApp")
				resolver.trigger()
			case *events.LoggedOut:
				logger.Warnf("Device logged out; start pairing again to log in")
			}
		})
		return rc
	}
	pm = newPairingManager(buildClient(deviceStore), func() (waClient, error) {
		return buildClient(container.NewDevice()), nil
	})
	if stderrIsTTY() {
		pm.printQR = func(code string) {
			logln("\nScan this QR code with your WhatsApp app:")
			qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stderr)
		}
	}

	// Start the REST API before connecting; handlers answer "Not connected" until the client is up
	idle := newIdleTracker()
	api := newAPIServer(pm.Client, messageStore, token, bridgeAddr())
	api.pairing = pm
	api.idle = idle
	restServer, _, err := startRESTServer(bridgeAddr(), api.handler())
	if err != nil {
		logger.Errorf("Failed to start REST API server: %v", err)
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := restServer.Shutdown(ctx); err != nil {
			logger.Warnf("REST API shutdown: %v", err)
		}
	}()

	if c := pm.current(); c.Paired() {
		// Already logged in, just connect; failures stay visible in /api/status
		if err := c.Connect(); err != nil {
			logger.Errorf("Failed to connect: %v", err)
		}
	} else if stderrIsTTY() {
		// Interactive run: show a QR code right away. Headless runs wait for an explicit request.
		if _, err := pm.StartPairing(); err != nil {
			logger.Errorf("Failed to start pairing: %v", err)
		}
	}

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)
	idleCtx, stopIdle := context.WithCancel(context.Background())
	defer stopIdle()
	idleExit := make(chan struct{}, 1)
	watchIdle(idleCtx, idle, idleExitDuration(), func() {
		select {
		case idleExit <- struct{}{}:
		default:
		}
	})

	logln("REST server is running. Press Ctrl+C to disconnect and exit.")
	select {
	case <-exitChan:
	case <-idleExit:
		logger.Infof("No API activity for %s; exiting", idleExitDuration())
	}

	logln("Disconnecting...")
	pm.current().Disconnect()
	return 0
}

// selfUser returns the bare user digits of the logged-in account ("" before pairing).
func selfUser(client *whatsmeow.Client) string {
	if client.Store.ID == nil {
		return ""
	}
	return client.Store.ID.User
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
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		// Try to parse the JID
		origJID, err := types.ParseJID(*conversation.ID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", *conversation.ID, err)
			continue
		}

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

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
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

				// Log the message content for debugging
				logger.Infof("Message: %d text bytes, Media Type: %v", len(content), mediaType)

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

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log metadata only, never message bodies or file names
					logger.Infof("Stored message: [%s] %s -> %s: %d text bytes, media %q",
						timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, len(content), mediaType)
				}
			}
		}
	}

	logf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		logln("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		logln("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
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
