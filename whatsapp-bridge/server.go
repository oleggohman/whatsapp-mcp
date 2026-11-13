package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const (
	defaultBridgeAddr = "127.0.0.1:8080"
	maxRequestBody    = 1 << 20 // 1 MB cap for JSON request bodies
)

// maxMediaBytes caps the size of a file that may be sent (a var so tests can lower it).
var maxMediaBytes int64 = 100 << 20

// bridgeClient is the subset of *whatsmeow.Client used by the REST handlers,
// so tests can substitute a fake.
type bridgeClient interface {
	IsConnected() bool
	Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// Function to send a WhatsApp message
// sentMessage is what the send path knows about a message WhatsApp accepted.
type sentMessage struct {
	chat types.JID
	resp whatsmeow.SendResponse
	msg  *waE2E.Message
}

func sendWhatsAppMessage(client bridgeClient, recipient string, message string, mediaPath string) (bool, string) {
	ok, text, _ := sendAndDescribe(client, recipient, message, mediaPath)
	return ok, text
}

// sendAndDescribe sends a message and, on success, returns what was sent so it can be stored.
func sendAndDescribe(client bridgeClient, recipient string, message string, mediaPath string) (bool, string, *sentMessage) {
	if client == nil || !client.IsConnected() {
		return false, "Not connected to WhatsApp", nil
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err), nil
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waE2E.Message{}

	// Check if we have media to send
	if mediaPath != "" {
		// Validate and read through one open file, so every caller gets the same guarantees
		// as the REST handler and a swap after validation cannot slip anything past them
		mediaData, err := readMediaFile(mediaPath)
		if err != nil {
			return false, err.Error(), nil
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err), nil
		}

		logf("Media uploaded (%d bytes)\n", resp.FileLength)

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waE2E.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Only .ogg maps to audio, so the file is always analyzed as Ogg Opus
			seconds, waveform, err := analyzeOggOpus(mediaData)
			if err != nil {
				return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err), nil
			}

			msg.AudioMessage = &waE2E.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waE2E.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waE2E.DocumentMessage{
				Title:         proto.String(mediaPath[strings.LastIndex(mediaPath, "/")+1:]),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	resp, err := client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err), nil
	}

	return true, fmt.Sprintf("Message sent to %s", recipient), &sentMessage{chat: recipientJID.ToNonAD(), resp: resp, msg: msg}
}

// sentRecorder routes a sent message to the store of the client that sent it. whatsmeow
// emits no event for messages this device sends, so without it they never reach messages.db.
type sentRecorder struct{ handlers sync.Map } // bridgeClient -> func(*sentMessage)

// register sets how messages sent by c are stored.
func (r *sentRecorder) register(c bridgeClient, store func(*sentMessage)) { r.handlers.Store(c, store) }

// record stores sent with the handler registered for c; false when there is none.
func (r *sentRecorder) record(c bridgeClient, sent *sentMessage) bool {
	h, ok := r.handlers.Load(c)
	if !ok || sent == nil {
		return false
	}
	h.(func(*sentMessage))(sent)
	return true
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// validateMediaPath ensures a media path is absolute, points at a regular file and is not too big.
func validateMediaPath(p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("media_path must be an absolute path")
	}
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("media_path is not accessible: %v", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("media_path must be a regular file")
	}
	if info.Size() > maxMediaBytes {
		return fmt.Errorf("media file too large (%d bytes, max %d)", info.Size(), maxMediaBytes)
	}
	return nil
}

// mediaFile is the part of *os.File that readMediaFile uses.
type mediaFile interface {
	io.ReadCloser
	Stat() (os.FileInfo, error)
}

// openMediaFile opens p for reading; a seam so tests can inject read and stat failures.
// O_NONBLOCK: opening a FIFO must not wait for a writer (it is rejected after the open).
var openMediaFile = func(p string) (mediaFile, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: checked by readMediaFile
	if err != nil {
		return nil, err
	}
	return f, nil
}

// readMediaFile applies the validateMediaPath rules to the file it actually opens and reads
// at most maxMediaBytes from it, so the path cannot be swapped for a FIFO, a device or a
// bigger file between the check and the read.
func readMediaFile(p string) ([]byte, error) {
	if !filepath.IsAbs(p) {
		return nil, errors.New("media_path must be an absolute path")
	}
	f, err := openMediaFile(p)
	if err != nil {
		return nil, fmt.Errorf("media_path is not accessible: %v", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("media_path is not accessible: %v", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("media_path must be a regular file")
	}
	if info.Size() > maxMediaBytes {
		return nil, fmt.Errorf("media file too large (%d bytes, max %d)", info.Size(), maxMediaBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMediaBytes+1))
	if err != nil {
		return nil, fmt.Errorf("Error reading media file: %v", err)
	}
	if int64(len(data)) > maxMediaBytes {
		return nil, fmt.Errorf("media file too large (more than %d bytes)", maxMediaBytes)
	}
	return data, nil
}

// bridgeAddr returns the configured REST listen address (loopback by default).
func bridgeAddr() string {
	if a := os.Getenv("WHATSAPP_BRIDGE_ADDR"); a != "" {
		return a
	}
	return defaultBridgeAddr
}

// apiServer holds what the REST handlers need. The client is fetched per request
// so it can be swapped (or absent) without rebuilding the handler.
type apiServer struct {
	client       func() bridgeClient
	messageStore *MessageStore
	token        string   // optional bearer token; empty disables auth
	extraHosts   []string // additional allowed Host names (configured bind host)

	pairing     *pairingManager // status/pairing endpoints; nil answers 503
	idle        *idleTracker    // optional; touched by authenticated non-health requests
	storeDirAbs string          // absolute store dir reported by /api/health
	shutdown    chan struct{}   // signalled by /api/shutdown; buffered(1), created only in newAPIServer
	sent        sentRecorder    // stores messages sent through /api/send
}

// route is one API endpoint plus its guard policy.
type route struct {
	pattern string
	handler http.HandlerFunc
	auth    bool // requires the bearer token when one is configured
}

// newAPIServer builds an apiServer; addr is the configured listen address, whose host
// (when it is a concrete name/IP) joins the Host allowlist.
func newAPIServer(client func() bridgeClient, store *MessageStore, token, addr string) *apiServer {
	a := &apiServer{client: client, messageStore: store, token: token, shutdown: make(chan struct{}, 1)}
	if abs, err := filepathAbs(storeDir()); err == nil {
		a.storeDirAbs = abs
	} else {
		a.storeDirAbs = storeDir()
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsUnspecified() {
			a.extraHosts = append(a.extraHosts, strings.ToLower(host))
		}
	}
	return a
}

// routes lists all endpoints; later tasks add entries here with their own auth flag.
func (a *apiServer) routes() []route {
	return []route{
		{pattern: "/api/health", handler: a.handleHealth, auth: false},
		{pattern: "/api/status", handler: a.handleStatus, auth: true},
		{pattern: "/api/start-pairing", handler: a.handleStartPairing, auth: true},
		{pattern: "/api/reconnect", handler: a.handleReconnect, auth: true},
		{pattern: "/api/pair-phone", handler: a.handlePairPhone, auth: true},
		{pattern: "/api/send", handler: a.handleSend, auth: true},
		{pattern: "/api/download", handler: a.handleDownload, auth: true},
		{pattern: "/api/shutdown", handler: a.handleShutdown, auth: true},
	}
}

// handler returns the guarded mux.
func (a *apiServer) handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range a.routes() {
		mux.Handle(rt.pattern, a.guard(rt.auth, rt.handler))
	}
	return mux
}

// hostAllowed reports whether the request Host names this machine (DNS-rebinding defence).
func (a *apiServer) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	for _, h := range a.extraHosts {
		if host == strings.Trim(h, "[]") {
			return true
		}
	}
	return false
}

// tokenOK compares the bearer token in constant time.
func (a *apiServer) tokenOK(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	want := sha256.Sum256([]byte(a.token))
	have := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(want[:], have[:]) == 1
}

// guard rejects requests that could come from a browser or a rebinding attack and
// enforces the optional token on routes that need it.
func (a *apiServer) guard(auth bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.hostAllowed(r.Host) {
			http.Error(w, "Forbidden host", http.StatusForbidden)
			return
		}
		// Legit clients (MCP server, curl) never send Origin; browsers always do on cross-site POSTs
		if r.Header.Get("Origin") != "" {
			http.Error(w, "Browser-originated requests are not allowed", http.StatusForbidden)
			return
		}
		if auth && a.token != "" && !a.tokenOK(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		// Health polls (liveness checks by the supervisor) must not keep an idle bridge alive
		if a.idle != nil && r.URL.Path != "/api/health" {
			a.idle.Touch()
		}
		// Only requests with bodies need the JSON content type; it forces a CORS preflight in browsers
		if r.Method == http.MethodPost {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next(w, r)
	}
}

// writeJSON writes v as JSON with the given status, logging encode failures.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logf("Failed to write response: %v\n", err)
	}
}

// requestShutdown asks run() to exit; repeated calls are no-ops and never block.
func (a *apiServer) requestShutdown() {
	select {
	case a.shutdown <- struct{}{}:
	default:
	}
}

// shutdownRequest is the body of POST /api/shutdown.
type shutdownRequest struct {
	Version string `json:"version"`
}

// handleShutdown handles POST /api/shutdown. It only obeys a caller that names the exact
// version of this build, so a supervisor cannot stop a bridge it did not mean to replace.
func (a *apiServer) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req shutdownRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}
	if req.Version != bridgeVersion {
		http.Error(w, "Version mismatch", http.StatusConflict)
		return
	}
	logln("Shutdown requested via API")
	writeJSON(w, http.StatusOK, SendMessageResponse{Success: true, Message: "Shutting down"})
	a.requestShutdown()
}

// handleSend handles POST /api/send.
func (a *apiServer) handleSend(w http.ResponseWriter, r *http.Request) {
	// Only allow POST requests
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the request body
	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	// Validate request
	if req.Recipient == "" {
		http.Error(w, "Recipient is required", http.StatusBadRequest)
		return
	}

	if req.Message == "" && req.MediaPath == "" {
		http.Error(w, "Message or media path is required", http.StatusBadRequest)
		return
	}

	if req.MediaPath != "" {
		if err := validateMediaPath(req.MediaPath); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Log metadata only, never message bodies
	logf("Received request to send message (text %d bytes, media %t)\n", len(req.Message), req.MediaPath != "")

	// Send the message
	client := a.client()
	success, message, sent := sendAndDescribe(client, req.Recipient, req.Message, req.MediaPath)
	logln("Message sent", success)
	if sent != nil && !a.sent.record(client, sent) {
		logln("Sent message not stored: no store handler for this client")
	}

	status := http.StatusOK
	if !success {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, SendMessageResponse{Success: success, Message: message})
}

// handleDownload handles POST /api/download.
func (a *apiServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	// Only allow POST requests
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the request body
	var req DownloadMediaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	// Validate request
	if req.MessageID == "" || req.ChatJID == "" {
		http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
		return
	}

	// Download the media
	success, mediaType, filename, path, err := downloadMedia(a.client(), a.messageStore, req.MessageID, req.ChatJID)

	// Handle download result
	if !success || err != nil {
		errMsg := "Unknown error"
		if err != nil {
			errMsg = err.Error()
		}
		writeJSON(w, http.StatusInternalServerError, DownloadMediaResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to download media: %s", errMsg),
		})
		return
	}

	writeJSON(w, http.StatusOK, DownloadMediaResponse{
		Success:  true,
		Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
		Filename: filename,
		Path:     path,
	})
}

// restShutdownTimeout bounds the graceful REST shutdown. http.Server.Shutdown only treats a
// connection that never sent a request (StateNew) as idle once it is 5 s old, so anything
// at or below 5 s can expire on such a connection.
var restShutdownTimeout = 10 * time.Second

// shutdownRESTServer shuts srv down gracefully within timeout and then force-closes whatever
// is left, so no connection outlives the bridge's exit. It returns the graceful error.
func shutdownRESTServer(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close() // force-close what the graceful shutdown left
	}
	return err
}

// listenTCP is a seam so tests can inject a failing listener.
var listenTCP = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

// startRESTServer binds addr synchronously (so bind errors surface) and serves handler
// in the background. Call Shutdown on the returned server to stop it.
func startRESTServer(addr string, handler http.Handler) (*http.Server, net.Addr, error) {
	ln, err := listenTCP(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	logf("Starting REST API server on %s...\n", ln.Addr())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("REST API server error: %v\n", err)
		}
	}()
	return srv, ln.Addr(), nil
}
