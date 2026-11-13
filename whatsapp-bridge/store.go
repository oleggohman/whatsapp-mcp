package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// defaultStoreDir is used when WHATSAPP_STORE_DIR is not set.
const defaultStoreDir = "store"

// storeDir returns the directory holding messages.db, whatsapp.db and downloaded media.
func storeDir() string {
	if dir := os.Getenv("WHATSAPP_STORE_DIR"); dir != "" {
		return dir
	}
	return defaultStoreDir
}

// sqliteDSN builds a go-sqlite3 file: DSN for path, escaping URI-special characters.
func sqliteDSN(path string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + escaped + "?_foreign_keys=on&_busy_timeout=5000"
}

// messagesDSN is sqliteDSN plus immediate write transactions and WAL, so the repair
// pass and the Python reader (mode=ro) do not block each other.
func messagesDSN(path string) string {
	return sqliteDSN(path) + "&_txlock=immediate&_journal_mode=WAL"
}

// messagesDBPath returns the location of messages.db.
func messagesDBPath() string {
	return filepath.Join(storeDir(), "messages.db")
}

// openMessagesDB opens messages.db with the standard DSN options.
var openMessagesDB = func(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", messagesDSN(path))
}

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	// 0700: the store holds the WhatsApp session, the API token and private messages
	if err := os.MkdirAll(storeDir(), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := openMessagesDB(messagesDBPath())
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
	`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages 
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return chats, nil
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// touchChat moves a chat's last message time to ts, creating the chat with name when missing.
// Unlike StoreChat it never overwrites a known chat name.
func (store *MessageStore) touchChat(jid, name string, ts time.Time) error {
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET last_message_time = excluded.last_message_time`,
		jid, name, ts)
	return err
}

// getChatName returns the stored name of a chat ("" if the chat is missing).
func (store *MessageStore) getChatName(jid string) string {
	var name sql.NullString
	if err := store.db.QueryRow("SELECT name FROM chats WHERE jid = ?", jid).Scan(&name); err != nil {
		return ""
	}
	return name.String
}

// updateChatName sets the name of an existing chat.
func (store *MessageStore) updateChatName(jid, name string) error {
	_, err := store.db.Exec("UPDATE chats SET name = ? WHERE jid = ?", name, jid)
	return err
}

// chatsWithName lists (jid, name) of all chats, optionally restricted to a JID suffix such as "@lid".
func (store *MessageStore) chatsWithName(suffix string) ([][2]string, error) {
	rows, err := store.db.Query("SELECT jid, COALESCE(name, '') FROM chats WHERE jid LIKE ?", "%"+suffix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out [][2]string
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err != nil {
			return nil, err
		}
		out = append(out, [2]string{jid, name})
	}
	return out, rows.Err()
}

// mergeChat moves everything stored under the LID chat to the PN chat in one transaction
// and removes the LID chat. It is idempotent: a missing LID chat is a no-op.
func (store *MessageStore) mergeChat(ctx context.Context, lid, pn string, lids store.LIDStore) error {
	lidJ, err := types.ParseJID(lid)
	if err != nil {
		return fmt.Errorf("parse lid jid: %w", err)
	}
	pnJ, err := types.ParseJID(pn)
	if err != nil {
		return fmt.Errorf("parse pn jid: %w", err)
	}

	// Resolve sender mappings before the write transaction so no other lookup runs inside it.
	senderRows, err := store.db.QueryContext(ctx,
		"SELECT DISTINCT sender FROM messages WHERE chat_jid IN (?, ?)", lid, pn)
	if err != nil {
		return err
	}
	senderMap := map[string]string{}
	var senders []string
	for senderRows.Next() {
		var s sql.NullString
		if err := senderRows.Scan(&s); err != nil {
			_ = senderRows.Close()
			return err
		}
		if s.String != "" {
			senders = append(senders, s.String)
		}
	}
	if err := senderRows.Err(); err != nil {
		_ = senderRows.Close()
		return err
	}
	_ = senderRows.Close()
	if lids != nil {
		for _, s := range senders {
			mapped, err := lids.GetPNForLID(ctx, types.NewJID(s, types.HiddenUserServer))
			if err == nil && !mapped.IsEmpty() && mapped.User != s {
				senderMap[s] = mapped.User
			}
		}
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var lidName sql.NullString
	var lidTime sql.NullTime
	err = tx.QueryRowContext(ctx, "SELECT name, last_message_time FROM chats WHERE jid = ?", lid).Scan(&lidName, &lidTime)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	// 1. PN chat row must exist before messages move (foreign key).
	if _, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)", pn, lidName, lidTime); err != nil {
		return err
	}
	// 2. Move messages; rows whose (id, chat_jid) already exist under the PN chat are skipped.
	if _, err := tx.ExecContext(ctx, "UPDATE OR IGNORE messages SET chat_jid = ? WHERE chat_jid = ?", pn, lid); err != nil {
		return err
	}
	// 3. Whatever is still under the LID chat is a duplicate of a PN row.
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE chat_jid = ?", lid); err != nil {
		return err
	}
	// 4. Map LID senders to PN digits.
	for from, to := range senderMap {
		if _, err := tx.ExecContext(ctx, "UPDATE messages SET sender = ? WHERE chat_jid = ? AND sender = ?", to, pn, from); err != nil {
			return err
		}
	}
	// 5. Best name and latest time on the PN chat.
	var pnName sql.NullString
	var pnTime sql.NullTime
	if err := tx.QueryRowContext(ctx, "SELECT name, last_message_time FROM chats WHERE jid = ?", pn).Scan(&pnName, &pnTime); err != nil {
		return err
	}
	best := pnName.String
	if isPlaceholderName(best, pnJ) && !isPlaceholderName(lidName.String, lidJ) {
		best = lidName.String
	}
	if best == "" {
		best = lidName.String
	}
	last := pnTime
	if lidTime.Valid && (!last.Valid || lidTime.Time.After(last.Time)) {
		last = lidTime
	}
	if _, err := tx.ExecContext(ctx, "UPDATE chats SET name = ?, last_message_time = ? WHERE jid = ?", best, last, pn); err != nil {
		return err
	}
	// 6. Drop the LID chat.
	if _, err := tx.ExecContext(ctx, "DELETE FROM chats WHERE jid = ?", lid); err != nil {
		return err
	}
	return tx.Commit()
}
