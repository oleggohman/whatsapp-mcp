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
// pass and the Python reader (mode=ro) do not block each other. With WAL,
// synchronous=NORMAL syncs on checkpoint instead of every commit: a process crash
// keeps committed transactions, a power loss can drop every commit since the last
// checkpoint. This store is a local mirror that history sync can fill again.
func messagesDSN(path string) string {
	return sqliteDSN(path) + "&_txlock=immediate&_journal_mode=WAL&_synchronous=NORMAL"
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
	// ftsCheck is the last integrity check of messages_fts that migrateSearch ran; run logs it.
	ftsCheck ftsCheck
}

// ftsCheck is how long an integrity check of messages_fts took and what it found (nil: healthy).
// ran is set once the check executed: took alone cannot tell, as a coarse clock (Windows) can report 0.
type ftsCheck struct {
	ran  bool
	took time.Duration
	err  error
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

// migrateIndexesSQL creates the btree indexes the read paths rely on.
// whatsapp-mcp-server/tests/conftest.py copies this, migrateFTSTableSQL and
// migrateFTSTriggersSQL; its test_schema_sync.py fails if they drift.
const migrateIndexesSQL = `
	CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages (chat_jid, timestamp);
	CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages (timestamp);
	CREATE INDEX IF NOT EXISTS idx_messages_sender_time ON messages (sender, timestamp);`

// migrateFTSTableSQL creates the external-content trigram index over messages.content.
// case_sensitive 0 (case folding) is the trigram default; it is spelled out so the intent is explicit.
// The index is keyed on messages.rowid, so a VACUUM (which can renumber rowids) requires
// INSERT INTO messages_fts(messages_fts) VALUES('rebuild') before search is correct.
// Nothing in this repo vacuums.
const migrateFTSTableSQL = `
	CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
		content,
		content='messages',
		content_rowid='rowid',
		tokenize='trigram case_sensitive 0'
	);`

// migrateFTSTriggersSQL keeps messages_fts in step with messages. It runs on every migration
// so a downgrade that dropped the triggers but left the table cannot leave later writes unindexed;
// migrateSearch also rebuilds the index whenever a trigger was missing.
const migrateFTSTriggersSQL = `
	CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
		INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
	END;
	CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
	END;
	CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE OF content ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
		INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
	END;`

// dropFTSTriggersSQL removes the triggers that write to messages_fts. DROP TRIGGER never opens
// the virtual table, so it works even when FTS5 can no longer read the index.
const dropFTSTriggersSQL = `
	DROP TRIGGER IF EXISTS messages_fts_insert;
	DROP TRIGGER IF EXISTS messages_fts_delete;
	DROP TRIGGER IF EXISTS messages_fts_update;`

// migrateSearch creates the message indexes and the FTS table with its triggers, and builds the
// FTS index when the table is new. An existing index is stale when a trigger is missing or it
// does not cover every message. A missing trigger means writes went unindexed: an older bridge's
// INSERT OR REPLACE renumbers rowids, so the index can be stale even at an equal row count.
// An index FTS5 cannot read, or one with segments that have no messages_fts_idx entry (MATCH
// would find nothing), is treated as stale too: its triggers would make every write to
// messages fail. See ftsHealthy for how that is checked. A stale index is dropped before the
// rebuild starts. Readers use MATCH whenever messages_fts exists with its three triggers, so a
// stale table kept while the rebuild runs, or after it fails or is cancelled, would silently miss
// rows instead of falling back to LIKE. The triggers are only created in the transaction that
// also creates the table or finds it healthy, so they never point at a broken index. Triggers
// left behind by a dropped table are removed before anything else, even when ctx is cancelled.
// The virtual table, the triggers and the build share one transaction: a crash rolls all of it
// back and the next start retries.
// Nothing writes messages while this runs: it runs before Connect.
func (store *MessageStore) migrateSearch(ctx context.Context) error {
	// Triggers without their table fail every write to messages, so this step ignores
	// cancellation: it is two quick statements, and a later step that fails can no longer leave them.
	safe := context.WithoutCancel(ctx)
	var exists, triggers int
	if err := store.db.QueryRowContext(safe, `SELECT
		(SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts'),
		(SELECT count(*) FROM sqlite_master WHERE type = 'trigger'
			AND name IN ('messages_fts_insert', 'messages_fts_delete', 'messages_fts_update'))`).Scan(&exists, &triggers); err != nil {
		return fmt.Errorf("check messages_fts: %w", err)
	}
	if exists == 0 && triggers > 0 {
		if _, err := store.db.ExecContext(safe, dropFTSTriggersSQL); err != nil {
			return fmt.Errorf("drop orphaned messages_fts triggers: %w", err)
		}
	}
	if _, err := store.db.ExecContext(ctx, migrateIndexesSQL); err != nil {
		return fmt.Errorf("create indexes: %w", err)
	}
	if exists > 0 {
		healthy, err := store.ftsHealthy(ctx, triggers)
		if err != nil {
			return fmt.Errorf("check messages_fts: %w", err)
		}
		if !healthy {
			if err := store.dropFTS(ctx); err != nil {
				return fmt.Errorf("drop stale messages_fts: %w", err)
			}
			exists = 0
		}
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, migrateFTSTableSQL); err != nil {
		return fmt.Errorf("create messages_fts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, migrateFTSTriggersSQL); err != nil {
		return fmt.Errorf("create messages_fts triggers: %w", err)
	}
	if exists == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("rebuild messages_fts: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ftsSegmentsWithoutIdxSQL counts index segments that have no row in messages_fts_idx. FTS5 finds
// a term's leaf through messages_fts_idx, so a segment without rows there silently returns no
// hits, and neither integrity-check form notices: they only verify rows that exist. A healthy
// segment always has at least one idx row, because the segment writer starts with iBtPage = 1 and
// so records its first leaf. Segment leaves live in messages_fts_data under id = segid << 37 | ...
// (FTS5_DATA_PAGE_B 31 + FTS5_DATA_HEIGHT_B 5 + FTS5_DATA_DLI_B 1); ids 1 and 10 are the averages
// and structure records and sit below 1 << 37. The recursion hops from one segment id to the next
// with one index seek each, so it never scans the table.
// This depends on the FTS5 on-disk layout and assumes messages_fts does not use contentless_delete:
// that option stores tombstone pages under segid + 65536 (FTS5_TOMBSTONE_ROWID), which this query
// would read as segments without idx rows. messages_fts is an external-content table, so it has
// none; revisit the query if that ever changes.
const ftsSegmentsWithoutIdxSQL = `
	WITH RECURSIVE seg(id) AS (
		SELECT min(id) >> 37 FROM messages_fts_data WHERE id >= (1 << 37)
		UNION ALL
		SELECT (SELECT min(id) >> 37 FROM messages_fts_data WHERE id >= ((seg.id + 1) << 37))
		FROM seg WHERE seg.id IS NOT NULL
	)
	SELECT count(*) FROM seg
	WHERE id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM messages_fts_idx WHERE segid = seg.id)`

// ftsHealthy reports whether the existing messages_fts can be kept: all three triggers are
// there, the docsize shadow table counts every message, no index segment lacks its
// messages_fts_idx entry, and FTS5 can read the whole index.
// External-content count(*) on messages_fts reads messages, so docsize is what says how many rows
// are really indexed. The probe only opens the virtual table (it reads messages_fts_config); the
// segments in messages_fts_data and messages_fts_idx, which the triggers write, are only read by
// the checks below. The segment check is cheap and runs first, because the integrity check passes
// an index whose idx rows are gone while MATCH finds nothing. The integrity check runs with
// rank 1, which also rebuilds every row's tokens from messages and compares them with the index:
// the plain check passes a damaged averages record (id 1) or structure record (id 10) that then
// fails writes. It scans the whole index and messages, so it runs last. The duration of both
// checks is recorded in store.ftsCheck. Any failure, including an error from the segment query,
// means stale; the error is only returned when ctx was cancelled, since an interrupted check
// says nothing about the index.
func (store *MessageStore) ftsHealthy(ctx context.Context, triggers int) (bool, error) {
	var indexed, total, probe int
	err := store.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM messages_fts_docsize), (SELECT count(*) FROM messages),
		(SELECT count(*) FROM (SELECT 1 FROM messages_fts LIMIT 1))`).Scan(&indexed, &total, &probe)
	if err == nil && triggers == 3 && indexed == total {
		start := time.Now()
		var orphaned int
		err = store.db.QueryRowContext(ctx, ftsSegmentsWithoutIdxSQL).Scan(&orphaned)
		if err == nil && orphaned > 0 {
			err = fmt.Errorf("%d index segments have no entry in messages_fts_idx", orphaned)
		}
		if err == nil {
			_, err = store.db.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts, rank) VALUES('integrity-check', 1)`)
		}
		store.ftsCheck = ftsCheck{ran: true, took: time.Since(start), err: err}
		if err == nil {
			return true, nil
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, nil
}

// dropFTS drops the triggers first and lets them commit on their own: from then on writes to
// messages no longer touch the index, and readers ignore a table without its triggers. Then it
// drops the table. FTS5 must open a table to drop it, so a broken one stays behind; it is
// harmless without triggers, and the next start tries again. run logs the error and connects.
func (store *MessageStore) dropFTS(ctx context.Context) error {
	if _, err := store.db.ExecContext(ctx, dropFTSTriggersSQL); err != nil {
		return fmt.Errorf("triggers: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE IF EXISTS messages_fts`); err != nil {
		return fmt.Errorf("table: %w", err)
	}
	return nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// upsertMessageSQL inserts a message or updates it in place. Unlike INSERT OR REPLACE it keeps
// the row's rowid stable, which an external-content FTS index keyed on rowid relies on.
const upsertMessageSQL = `INSERT INTO messages
	(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id, chat_jid) DO UPDATE SET
		sender = excluded.sender,
		content = excluded.content,
		timestamp = excluded.timestamp,
		is_from_me = excluded.is_from_me,
		media_type = excluded.media_type,
		filename = excluded.filename,
		url = excluded.url,
		media_key = excluded.media_key,
		file_sha256 = excluded.file_sha256,
		file_enc_sha256 = excluded.file_enc_sha256,
		file_length = excluded.file_length`

// upsertHistoryChatSQL upserts a chat for a history batch. The stored last_message_time only
// moves forward (compared as stored text, like every timestamp in this table).
const upsertHistoryChatSQL = `INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
	ON CONFLICT(jid) DO UPDATE SET
		name = excluded.name,
		last_message_time = CASE
			WHEN chats.last_message_time IS NULL
			  OR chats.last_message_time < excluded.last_message_time
			THEN excluded.last_message_time
			ELSE chats.last_message_time
		END`

// historyMessage is one history-sync message ready to be stored.
type historyMessage struct {
	id, sender, content                 string
	timestamp                           time.Time
	isFromMe                            bool
	mediaType, filename, url            string
	mediaKey, fileSHA256, fileEncSHA256 []byte
	fileLength                          uint64
}

// storeHistoryBatch writes one conversation in a single transaction: the chat upsert first
// (the foreign key needs the chat row), then every message. Any failure rolls everything back.
func (store *MessageStore) storeHistoryBatch(chatJID, name string, latest time.Time, msgs []historyMessage) error {
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(upsertHistoryChatSQL, chatJID, name, latest); err != nil {
		return fmt.Errorf("store chat: %w", err)
	}
	stmt, err := tx.Prepare(upsertMessageSQL)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, m := range msgs {
		if _, err := stmt.Exec(m.id, chatJID, m.sender, m.content, m.timestamp, m.isFromMe,
			m.mediaType, m.filename, m.url, m.mediaKey, m.fileSHA256, m.fileEncSHA256, m.fileLength); err != nil {
			return fmt.Errorf("store message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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
		upsertMessageSQL,
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
