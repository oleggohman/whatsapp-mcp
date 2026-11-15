package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func newTestStore(t *testing.T) *MessageStore {
	t.Helper()
	t.Setenv("WHATSAPP_STORE_DIR", t.TempDir())
	s, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreDir(t *testing.T) {
	t.Setenv("WHATSAPP_STORE_DIR", "")
	if got := storeDir(); got != "store" {
		t.Fatalf("default store dir = %q, want store", got)
	}
	t.Setenv("WHATSAPP_STORE_DIR", "/some/where")
	if got := storeDir(); got != "/some/where" {
		t.Fatalf("store dir = %q", got)
	}
}

func TestSqliteDSNEscapes(t *testing.T) {
	got := sqliteDSN("/a b/c?d#e%f/messages.db")
	want := "file:/a b/c%3fd%23e%25f/messages.db?_foreign_keys=on&_busy_timeout=5000"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNewMessageStoreUsesStoreDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	t.Setenv("WHATSAPP_STORE_DIR", dir)
	s, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(filepath.Join(dir, "messages.db")); err != nil {
		t.Fatalf("messages.db not created in store dir: %v", err)
	}
}

func TestStoreChatUpsertAndGetChats(t *testing.T) {
	s := newTestStore(t)
	t1 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	if err := s.StoreChat("a@s.whatsapp.net", "Alice", t1); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreChat("b@s.whatsapp.net", "Bob", t2); err != nil {
		t.Fatal(err)
	}
	// Upsert replaces the existing row.
	if err := s.StoreChat("a@s.whatsapp.net", "Alice 2", t2.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	chats, err := s.GetChats()
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 {
		t.Fatalf("want 2 chats, got %d", len(chats))
	}
	if !chats["a@s.whatsapp.net"].Equal(t2.Add(time.Hour)) {
		t.Fatalf("upsert did not update time: %v", chats["a@s.whatsapp.net"])
	}
	var name string
	if err := s.db.QueryRow("SELECT name FROM chats WHERE jid = ?", "a@s.whatsapp.net").Scan(&name); err != nil || name != "Alice 2" {
		t.Fatalf("name = %q, err = %v", name, err)
	}
}

func TestGetChatsEmpty(t *testing.T) {
	s := newTestStore(t)
	chats, err := s.GetChats()
	if err != nil || len(chats) != 0 {
		t.Fatalf("chats=%v err=%v", chats, err)
	}
}

func TestStoreAndGetMessages(t *testing.T) {
	s := newTestStore(t)
	chat := "a@s.whatsapp.net"
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.StoreChat(chat, "Alice", base); err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{"one", "two", "three"} {
		err := s.StoreMessage("id"+content, chat, "alice", content, base.Add(time.Duration(i)*time.Minute), i == 1, "", "", "", nil, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Empty content without media is skipped.
	if err := s.StoreMessage("empty", chat, "alice", "", base, false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.GetMessages(chat, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	// Newest first.
	if msgs[0].Content != "three" || msgs[2].Content != "one" {
		t.Fatalf("unexpected order: %+v", msgs)
	}
	if !msgs[1].IsFromMe || msgs[0].IsFromMe {
		t.Fatalf("is_from_me wrong: %+v", msgs)
	}

	limited, err := s.GetMessages(chat, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limit: len=%d err=%v", len(limited), err)
	}

	none, err := s.GetMessages("nobody@s.whatsapp.net", 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown chat: %v %v", none, err)
	}
}

func TestStoreMessageUpsert(t *testing.T) {
	s := newTestStore(t)
	chat := "a@s.whatsapp.net"
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.StoreChat(chat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"first", "edited"} {
		if err := s.StoreMessage("m1", chat, "alice", content, ts, false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.GetMessages(chat, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "edited" {
		t.Fatalf("msgs=%+v err=%v", msgs, err)
	}
}

// The upsert must keep the rowid: an external-content FTS index is keyed on it.
func TestStoreMessageUpsertKeepsRowid(t *testing.T) {
	s := newTestStore(t)
	chat := "a@s.whatsapp.net"
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.StoreChat(chat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	rowid := func() int64 {
		var id int64
		if err := s.db.QueryRow("SELECT rowid FROM messages WHERE id = 'm1'").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if err := s.StoreMessage("m1", chat, "alice", "first", ts, false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	before := rowid()
	if err := s.StoreMessage("m1", chat, "alice", "edited", ts, false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if after := rowid(); after != before {
		t.Fatalf("rowid changed from %d to %d", before, after)
	}
}

func TestStoreMessageRequiresChat(t *testing.T) {
	s := newTestStore(t)
	// Foreign keys are on, so a message for an unknown chat is rejected.
	err := s.StoreMessage("m1", "ghost@s.whatsapp.net", "x", "hi", time.Now(), false, "", "", "", nil, nil, nil, 0)
	if err == nil {
		t.Fatal("expected foreign key error")
	}
}

func TestStoreAndGetMediaInfo(t *testing.T) {
	s := newTestStore(t)
	chat := "a@s.whatsapp.net"
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.StoreChat(chat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	key, sha, enc := []byte{1, 2, 3}, []byte{4, 5}, []byte{6}
	err := s.StoreMessage("m1", chat, "alice", "", ts, false, "image", "pic.jpg", "https://x/y.enc", key, sha, enc, 42)
	if err != nil {
		t.Fatal(err)
	}

	mt, fn, url, k, h, e, l, err := s.GetMediaInfo("m1", chat)
	if err != nil {
		t.Fatal(err)
	}
	if mt != "image" || fn != "pic.jpg" || url != "https://x/y.enc" || l != 42 ||
		string(k) != string(key) || string(h) != string(sha) || string(e) != string(enc) {
		t.Fatalf("unexpected media info: %v %v %v %v %v %v %v", mt, fn, url, k, h, e, l)
	}

	// StoreMediaInfo updates the row in place.
	if err := s.StoreMediaInfo("m1", chat, "https://new", []byte{9}, []byte{8}, []byte{7}, 99); err != nil {
		t.Fatal(err)
	}
	_, _, url, k, _, _, l, err = s.GetMediaInfo("m1", chat)
	if err != nil || url != "https://new" || l != 99 || string(k) != string([]byte{9}) {
		t.Fatalf("after update: url=%q k=%v l=%d err=%v", url, k, l, err)
	}
}

func TestGetMediaInfoMissingRow(t *testing.T) {
	s := newTestStore(t)
	_, _, _, _, _, _, _, err := s.GetMediaInfo("nope", "a@s.whatsapp.net")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows, got %v", err)
	}
}

func TestMessagesDSNOptions(t *testing.T) {
	got := messagesDSN("/x/messages.db")
	want := "file:/x/messages.db?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate&_journal_mode=WAL&_synchronous=NORMAL"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNewMessageStoreUsesSynchronousNormal(t *testing.T) {
	s := newTestStore(t)
	var mode int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil || mode != 1 {
		t.Fatalf("synchronous = %d, %v", mode, err)
	}
}

func TestNewMessageStoreUsesWAL(t *testing.T) {
	s := newTestStore(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
}

func TestMigrateSearchCreatesIndexes(t *testing.T) {
	s := newTestStore(t)
	// NewMessageStore leaves the indexes to migrateSearch so opening stays fast.
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name LIKE 'idx_messages_%'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("indexes before migrateSearch = %d, %v", n, err)
	}
	// Running it twice keeps the IF NOT EXISTS statements idempotent.
	for i := 0; i < 2; i++ {
		if err := s.migrateSearch(context.Background()); err != nil {
			t.Fatalf("migrateSearch: %v", err)
		}
	}

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT content FROM messages
		WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?`, "a@s.whatsapp.net", 20)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "USING INDEX idx_messages_chat_time") &&
		!strings.Contains(plan, "USING COVERING INDEX idx_messages_chat_time") {
		t.Fatalf("plan does not use idx_messages_chat_time:\n%s", plan)
	}
}

// ftsCounts returns the rows really indexed (docsize) and the rows in messages.
func ftsCounts(t *testing.T, s *MessageStore) (indexed, total int) {
	t.Helper()
	if err := s.db.QueryRow("SELECT count(*) FROM messages_fts_docsize").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM messages").Scan(&total); err != nil {
		t.Fatal(err)
	}
	return indexed, total
}

// ftsHits returns the messages ids whose content matches the trigram query.
func ftsHits(t *testing.T, s *MessageStore, q string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id FROM messages WHERE rowid IN
		(SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?) ORDER BY id`, `"`+q+`"`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestMessagesFTSStaysInSyncWithMessages(t *testing.T) {
	s := newTestStore(t)
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	chat := "a@s.whatsapp.net"
	if err := s.StoreChat(chat, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	store := func(id, content string) {
		t.Helper()
		if err := s.StoreMessage(id, chat, "111", content, time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	check := func(step string, wantRows int, token string, want []string) {
		t.Helper()
		indexed, total := ftsCounts(t, s)
		if indexed != total || total != wantRows {
			t.Fatalf("%s: docsize = %d, messages = %d, want both %d", step, indexed, total, wantRows)
		}
		if got := ftsHits(t, s, token); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: MATCH %q = %v, want %v", step, token, got, want)
		}
	}

	store("m1", "alpha bravo")
	check("insert", 1, "bravo", []string{"m1"})
	check("insert mid-word upper-case", 1, "RAV", []string{"m1"})
	if _, err := s.db.Exec("UPDATE messages SET content = 'charlie delta' WHERE id = 'm1'"); err != nil {
		t.Fatal(err)
	}
	check("update", 1, "bravo", nil)
	check("update new token", 1, "delta", []string{"m1"})
	store("m1", "echo foxtrot") // upsert of the same key with new content
	check("upsert old token", 1, "delta", nil)
	check("upsert new token", 1, "foxtrot", []string{"m1"})
	store("m2", "golf hotel")
	check("second row", 2, "golf", []string{"m2"})
	if _, err := s.db.Exec("DELETE FROM messages WHERE id = 'm1'"); err != nil {
		t.Fatal(err)
	}
	check("delete", 1, "foxtrot", nil)
}

func TestMigrateSearchIndexesExistingRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.StoreChat("a@s.whatsapp.net", "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	for id, content := range map[string]string{"m1": "old row about pelican", "m2": "שלום עולם"} {
		if err := s.StoreMessage(id, "a@s.whatsapp.net", "111", content, time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'messages_fts'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("messages_fts in sqlite_master = %d, %v", n, err)
	}
	// The trigram tokenizer finds substrings, folds case and handles non-Latin text
	for q, want := range map[string]string{"pelican": "m1", "lica": "m1", "PELICAN": "m1", "לום": "m2"} {
		if got := ftsHits(t, s, q); len(got) != 1 || got[0] != want {
			t.Fatalf("MATCH %q = %v, want [%s]", q, got, want)
		}
	}
	// An index that diverged from messages (a crashed process) is rebuilt on the next start
	if _, err := s.db.Exec("INSERT INTO messages_fts(messages_fts) VALUES('delete-all')"); err != nil {
		t.Fatal(err)
	}
	if indexed, total := ftsCounts(t, s); indexed != 0 || total != 2 {
		t.Fatalf("after delete-all: docsize = %d, messages = %d", indexed, total)
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if indexed, total := ftsCounts(t, s); indexed != 2 || total != 2 {
		t.Fatalf("after rebuild: docsize = %d, messages = %d", indexed, total)
	}
	if got := ftsHits(t, s, "lica"); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("MATCH after rebuild = %v", got)
	}
}

// A downgrade drops the triggers but keeps messages_fts; the old bridge's INSERT OR REPLACE then
// renumbers rowids at an unchanged row count. The next migration must rebuild anyway.
func TestMigrateSearchRestoresDroppedTriggers(t *testing.T) {
	s := newTestStore(t)
	chat := "a@s.whatsapp.net"
	if err := s.StoreChat(chat, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	for id, content := range map[string]string{"m1": "pelican", "m2": "albatross"} {
		if err := s.StoreMessage(id, chat, "111", content, time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	names := []string{"messages_fts_insert", "messages_fts_delete", "messages_fts_update"}
	for _, n := range names {
		if _, err := s.db.Exec("DROP TRIGGER " + n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('m1', ?, '111', 'heron', ?, 0)`, chat, time.Now()); err != nil {
		t.Fatal(err)
	}
	if indexed, total := ftsCounts(t, s); indexed != total {
		t.Fatalf("setup: docsize = %d, messages = %d, want equal", indexed, total)
	}
	if got := ftsHits(t, s, "heron"); len(got) != 0 {
		t.Fatalf("setup: the replaced row must be unindexed, MATCH = %v", got)
	}

	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		var c int
		if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?", n).Scan(&c); err != nil || c != 1 {
			t.Fatalf("trigger %s present = %d, %v", n, c, err)
		}
	}
	if got := ftsHits(t, s, "heron"); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("MATCH heron after migration = %v", got)
	}
	if got := ftsHits(t, s, "pelican"); len(got) != 0 {
		t.Fatalf("stale MATCH pelican after migration = %v", got)
	}
	// The restored triggers index new writes
	if err := s.StoreMessage("m3", chat, "111", "osprey", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := ftsHits(t, s, "osprey"); len(got) != 1 || got[0] != "m3" {
		t.Fatalf("MATCH osprey = %v", got)
	}
}

// A cancelled migration returns the context error, leaves no FTS table or trigger behind,
// and the next start completes it.
func TestMigrateSearchCancelledLeavesNothingAndRetries(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.migrateSearch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	ftsObjects := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name LIKE 'messages_fts%'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := ftsObjects(); n != 0 {
		t.Fatalf("%d messages_fts objects after a cancelled migration", n)
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := ftsObjects(); n == 0 {
		t.Fatal("retry did not create messages_fts")
	}
}

// A stale index whose rebuild fails must not stay readable: readers would MATCH against it and
// miss rows instead of falling back to LIKE. Writes keep working and the next start rebuilds.
func TestMigrateSearchFailedRepairLeavesNoStaleIndex(t *testing.T) {
	s := newTestStore(t)
	// One connection, so the page limit below applies to the migration
	s.db.SetMaxOpenConns(1)
	chat := "a@s.whatsapp.net"
	if err := s.StoreChat(chat, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("pelican albatross heron osprey ", 40)
	for i := range 300 {
		if err := s.StoreMessage(fmt.Sprintf("m%d", i), chat, "111", fmt.Sprintf("%s %d", body, i), time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	// The index diverges (as after a crash), then the disk is full for the repair
	if _, err := s.db.Exec("INSERT INTO messages_fts(messages_fts) VALUES('delete-all')"); err != nil {
		t.Fatal(err)
	}
	// Compact the file so the freed index pages cannot hold the rebuild
	if _, err := s.db.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
		t.Fatal(err)
	}
	// SQLite reports the full disk at the rebuild or, in WAL mode, at commit
	if err := s.migrateSearch(context.Background()); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("err = %v, want a failed repair", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name LIKE 'messages_fts%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d messages_fts objects after a failed repair, want 0", n)
	}
	if _, err := s.db.Exec("PRAGMA max_page_count = 1073741823"); err != nil {
		t.Fatal(err)
	}
	// No trigger is left pointing at the dropped table
	if err := s.StoreMessage("late", chat, "111", "kingfisher", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if indexed, total := ftsCounts(t, s); indexed != 301 || total != 301 {
		t.Fatalf("after retry: docsize = %d, messages = %d, want 301", indexed, total)
	}
	if got := ftsHits(t, s, "kingfisher"); len(got) != 1 || got[0] != "late" {
		t.Fatalf("MATCH kingfisher = %v", got)
	}
}

// An index FTS5 can no longer open cannot be dropped. Its triggers must still go, or every write
// to messages would fail; without them readers use LIKE. Later starts retry the drop, never put
// the triggers back on the broken table, and the bridge keeps storing messages.
func TestMigrateSearchUnreadableIndexKeepsWritesWorking(t *testing.T) {
	for name, corrupt := range map[string][]string{
		// Stale (nothing indexed) and unreadable
		"stale": {"INSERT INTO messages_fts(messages_fts) VALUES('delete-all')", "DROP TABLE messages_fts_config"},
		// Counts and triggers look current, only the probe sees the damage
		"current counts": {"DROP TABLE messages_fts_config"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			chat := "a@s.whatsapp.net"
			if err := s.StoreChat(chat, "A", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := s.StoreMessage("m1", chat, "111", "pelican", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
				t.Fatal(err)
			}
			if err := s.migrateSearch(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, q := range corrupt {
				if _, err := s.db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			// A connection that already opened messages_fts keeps it open; use fresh ones like a restart
			s.db.SetMaxIdleConns(0)
			if err := s.StoreMessage("setup", chat, "111", "heron", time.Now(), false, "", "", "", nil, nil, nil, 0); err == nil {
				t.Fatal("setup: a write through the triggers must fail on the broken index")
			}
			ftsObjects := func() (triggers, tables int) {
				t.Helper()
				// The objects the MCP server's _fts_available counts
				if err := s.db.QueryRow(`SELECT
					(SELECT count(*) FROM sqlite_master WHERE type = 'trigger'
						AND name IN ('messages_fts_insert', 'messages_fts_delete', 'messages_fts_update')),
					(SELECT count(*) FROM sqlite_master WHERE name = 'messages_fts')`).Scan(&triggers, &tables); err != nil {
					t.Fatal(err)
				}
				return triggers, tables
			}
			for attempt := 1; attempt <= 2; attempt++ {
				err := s.migrateSearch(context.Background())
				if err == nil || !strings.Contains(err.Error(), "drop stale messages_fts: table: ") || !errors.Is(err, errFTSNotDroppable) {
					t.Fatalf("attempt %d: err = %v, want a failed table drop", attempt, err)
				}
				if triggers, tables := ftsObjects(); triggers != 0 || tables != 1 {
					t.Fatalf("attempt %d: %d triggers, %d messages_fts tables, want 0 and 1", attempt, triggers, tables)
				}
				id := fmt.Sprintf("live%d", attempt)
				if err := s.StoreMessage(id, chat, "111", "osprey", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
					t.Fatalf("attempt %d: StoreMessage: %v", attempt, err)
				}
				batch := []historyMessage{{id: id + "h", sender: "111", content: "albatross", timestamp: time.Now()}}
				if err := s.storeHistoryBatch(chat, "A", time.Now(), batch); err != nil {
					t.Fatalf("attempt %d: storeHistoryBatch: %v", attempt, err)
				}
			}
			var n int
			if err := s.db.QueryRow("SELECT count(*) FROM messages").Scan(&n); err != nil || n != 5 {
				t.Fatalf("messages = %d, %v, want 5", n, err)
			}
		})
	}
}

// ftsTriggers counts the messages_fts triggers the MCP server's _fts_available requires.
func ftsTriggers(t *testing.T, s *MessageStore) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'trigger'
		AND name IN ('messages_fts_insert', 'messages_fts_delete', 'messages_fts_update')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// checkWritesWork runs every write path that fires the messages_fts triggers: insert, upsert,
// a history batch, a content update and the LID merge (update and delete).
func checkWritesWork(t *testing.T, s *MessageStore, chat string) {
	t.Helper()
	// Removing a row from the index goes first: a successful insert rewrites the averages record
	if _, err := s.db.Exec("UPDATE messages SET content = content || ' grebe' WHERE rowid = (SELECT min(rowid) FROM messages)"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.db.Exec("DELETE FROM messages WHERE rowid = (SELECT max(rowid) FROM messages)"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for i := range 50 {
		if err := s.StoreMessage(fmt.Sprintf("w%d", i), chat, "111", fmt.Sprintf("kingfisher %d", i), time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatalf("StoreMessage %d: %v", i, err)
		}
	}
	if err := s.StoreMessage("w0", chat, "111", "kingfisher upserted", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	batch := []historyMessage{{id: "h1", sender: "111", content: "albatross", timestamp: time.Now()}}
	if err := s.storeHistoryBatch(chat, "A", time.Now(), batch); err != nil {
		t.Fatalf("storeHistoryBatch: %v", err)
	}
	if _, err := s.db.Exec("UPDATE messages SET content = 'cormorant' WHERE id = 'w1'"); err != nil {
		t.Fatalf("update: %v", err)
	}
	lid, pn := testLID+"@lid", testPN+"@s.whatsapp.net"
	for _, c := range []string{lid, pn} {
		if err := s.StoreChat(c, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.StoreMessage("dup", c, "111", "puffin", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatalf("StoreMessage %s: %v", c, err)
		}
	}
	if err := s.mergeChat(context.Background(), lid, pn, nil); err != nil {
		t.Fatalf("mergeChat: %v", err)
	}
}

// FTS5 opens an index whose segments or idx table are damaged, so the counts and the probe
// pass, but the triggers fail on the first write that touches the damage. The integrity check
// sees it: the index is dropped and rebuilt, and every write works afterwards.
func TestMigrateSearchDamagedIndexIsRebuilt(t *testing.T) {
	for name, corrupt := range map[string]string{
		"zeroed segment blocks":    "UPDATE messages_fts_data SET block = zeroblob(length(block)) WHERE id > 10",
		"garbage segment blocks":   "UPDATE messages_fts_data SET block = randomblob(length(block)) WHERE id > 10",
		"deleted segment rows":     "DELETE FROM messages_fts_data WHERE id > 10",
		"dropped messages_fts_idx": "DROP TABLE messages_fts_idx",
		// The averages record (id 1) and the structure record (id 10): only the content-checking
		// integrity check reads them through
		"zeroed averages record":  "UPDATE messages_fts_data SET block = zeroblob(length(block)) WHERE id = 1",
		"empty averages record":   "UPDATE messages_fts_data SET block = x'' WHERE id = 1",
		"garbage averages record": "UPDATE messages_fts_data SET block = x'ffffffffffffffffffffffffffffffff' WHERE id = 1",
		"deleted averages record": "DELETE FROM messages_fts_data WHERE id = 1",
		"zeroed structure record": "UPDATE messages_fts_data SET block = zeroblob(length(block)) WHERE id = 10",
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			chat := "a@s.whatsapp.net"
			if err := s.StoreChat(chat, "A", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := s.migrateSearch(context.Background()); err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("pelican albatross heron osprey ", 40)
			for i := range 400 {
				if err := s.StoreMessage(fmt.Sprintf("m%d", i), chat, "111", fmt.Sprintf("%s %d", body, i), time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(corrupt); err != nil {
				t.Fatal(err)
			}
			// A connection that already opened messages_fts keeps it open; use fresh ones like a restart
			s.db.SetMaxIdleConns(0)
			failed := 0
			// Damaged averages only show when the index removes a row (an upsert or a delete),
			// and a successful insert rewrites them, so these go first and the inserts only run
			// when they did not fail
			if err := s.StoreMessage("m0", chat, "111", "heron upserted", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
				failed++
			}
			if _, err := s.db.Exec("DELETE FROM messages WHERE id = 'm1'"); err != nil {
				failed++
			}
			for i := 0; i < 400 && failed == 0; i++ {
				if err := s.StoreMessage(fmt.Sprintf("setup%d", i), chat, "111", body, time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
					failed++
				}
			}
			if failed == 0 {
				t.Fatal("setup: writes through the triggers must fail on the damaged index")
			}

			if err := s.migrateSearch(context.Background()); err != nil {
				t.Fatalf("migrateSearch: %v", err)
			}
			checkWritesWork(t, s, chat)
			if s.ftsCheck.err == nil || !s.ftsCheck.ran {
				t.Fatalf("ftsCheck = %+v, want the failed integrity check that ran", s.ftsCheck)
			}
			if n := ftsTriggers(t, s); n != 3 {
				t.Fatalf("triggers = %d, want 3 on the rebuilt index", n)
			}
			if _, err := s.db.Exec(`INSERT INTO messages_fts(messages_fts, rank) VALUES('integrity-check', 1)`); err != nil {
				t.Fatalf("rebuilt index fails the integrity check: %v", err)
			}
			if indexed, total := ftsCounts(t, s); indexed != total {
				t.Fatalf("docsize = %d, messages = %d", indexed, total)
			}
			if got := ftsHits(t, s, "cormorant"); len(got) != 1 || got[0] != "w1" {
				t.Fatalf("MATCH cormorant = %v", got)
			}
			// Every row with the token, including the setup writes that got through
			var want int
			if err := s.db.QueryRow("SELECT count(*) FROM messages WHERE content LIKE '%osprey%'").Scan(&want); err != nil {
				t.Fatal(err)
			}
			if got := ftsHits(t, s, "osprey"); len(got) != want || want < 390 {
				t.Fatalf("MATCH osprey = %d hits, want %d (at least 390)", len(got), want)
			}
		})
	}
}

// A healthy index passes the integrity check, including rows with NULL or empty content, and
// is kept as it is: no drop, no rebuild.
func TestMigrateSearchHealthyIndexIsKept(t *testing.T) {
	s := newTestStore(t)
	chat := "a@s.whatsapp.net"
	if err := s.StoreChat(chat, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("m1", chat, "111", "pelican", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("m2", chat, "111", "", time.Now(), false, "image", "a.jpg", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	for id, content := range map[string]string{"h": "שלום עולם", "c": "Привет мир", "e": "🦩🦩 ok", "s": "ab"} {
		if err := s.StoreMessage(id, chat, "111", content, time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"DELETE FROM messages WHERE id = 's'",
		"UPDATE messages SET content = 'Пока мир' WHERE id = 'c'",
		`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('m3', 'a@s.whatsapp.net', '111', NULL, '2024-01-01', 0)`,
		"UPDATE messages SET content = NULL WHERE id = 'm1'",
		"UPDATE messages SET content = 'heron' WHERE id = 'm3'",
		// A rebuild recreates the table and resets this setting, so it marks the original index
		"INSERT INTO messages_fts(messages_fts, rank) VALUES('automerge', 7)",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.ftsCheck.err != nil || !s.ftsCheck.ran {
		t.Fatalf("ftsCheck = %+v, want a passed check that ran", s.ftsCheck)
	}
	var automerge int
	if err := s.db.QueryRow("SELECT v FROM messages_fts_config WHERE k = 'automerge'").Scan(&automerge); err != nil || automerge != 7 {
		t.Fatalf("automerge = %d, %v: the healthy index was rebuilt", automerge, err)
	}
	if n := ftsTriggers(t, s); n != 3 {
		t.Fatalf("triggers = %d, want 3", n)
	}
	if got := ftsHits(t, s, "heron"); len(got) != 1 || got[0] != "m3" {
		t.Fatalf("MATCH heron = %v", got)
	}
}

// The integrity check only verifies idx rows that exist, so an index that lost them passes both
// forms while MATCH silently misses rows. The segment check catches it and the index is rebuilt.
func TestMigrateSearchIndexWithoutIdxEntriesIsRebuilt(t *testing.T) {
	for name, damage := range map[string]string{
		"all idx rows":            "DELETE FROM messages_fts_idx",
		"idx rows of one segment": "DELETE FROM messages_fts_idx WHERE segid = (SELECT segid FROM messages_fts_idx GROUP BY segid ORDER BY count(*) DESC LIMIT 1)",
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			chat := "a@s.whatsapp.net"
			if err := s.migrateSearch(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Three transactions through the triggers give several segments of several leaves each
			rng := rand.New(rand.NewSource(1))
			for b := range 3 {
				batch := make([]historyMessage, 0, 1000)
				for i := range 1000 {
					words := make([]string, 0, 150)
					for len(words) < 150 {
						w := make([]byte, 3+rng.Intn(6))
						for j := range w {
							w[j] = byte('a' + rng.Intn(26))
						}
						words = append(words, string(w))
					}
					batch = append(batch, historyMessage{id: fmt.Sprintf("m%d-%d", b, i), sender: "111",
						content: "zebra " + strings.Join(words, " "), timestamp: time.Now()})
				}
				if err := s.storeHistoryBatch(chat, "A", time.Now(), batch); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(damage); err != nil {
				t.Fatal(err)
			}
			// A connection that already opened messages_fts keeps it open; use fresh ones like a restart
			s.db.SetMaxIdleConns(0)
			// The gap itself: both integrity-check forms pass, MATCH finds fewer rows than LIKE
			for _, rank := range []int{0, 1} {
				if _, err := s.db.Exec("INSERT INTO messages_fts(messages_fts, rank) VALUES('integrity-check', ?)", rank); err != nil {
					t.Fatalf("setup: integrity-check rank %d: %v", rank, err)
				}
			}
			var like int
			if err := s.db.QueryRow("SELECT count(*) FROM messages WHERE content LIKE '%zebra%'").Scan(&like); err != nil {
				t.Fatal(err)
			}
			if got := ftsHits(t, s, "zebra"); len(got) >= like {
				t.Fatalf("setup: MATCH zebra = %d hits, LIKE = %d, want fewer hits on the damaged index", len(got), like)
			}
			if err := s.StoreMessage("w", chat, "111", "heron", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
				t.Fatalf("setup: write on the damaged index: %v", err)
			}

			if err := s.migrateSearch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !s.ftsCheck.ran || s.ftsCheck.err == nil || !strings.Contains(s.ftsCheck.err.Error(), "no entry in messages_fts_idx") {
				t.Fatalf("ftsCheck = %+v, want the failed segment check", s.ftsCheck)
			}
			if n := ftsTriggers(t, s); n != 3 {
				t.Fatalf("triggers = %d, want 3 on the rebuilt index", n)
			}
			if err := s.db.QueryRow("SELECT count(*) FROM messages WHERE content LIKE '%zebra%'").Scan(&like); err != nil {
				t.Fatal(err)
			}
			if got := ftsHits(t, s, "zebra"); len(got) != like {
				t.Fatalf("MATCH zebra = %d hits, want %d", len(got), like)
			}
			if indexed, total := ftsCounts(t, s); indexed != total {
				t.Fatalf("docsize = %d, messages = %d", indexed, total)
			}
			// The rebuilt index is healthy: the next start keeps it
			if err := s.migrateSearch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !s.ftsCheck.ran || s.ftsCheck.err != nil {
				t.Fatalf("second start: ftsCheck = %+v, want a passed check", s.ftsCheck)
			}
		})
	}
}

// The segment check must not flag indexes that are healthy but look odd: no segments at all, a
// trigger-built index, and a merge that is only partly done.
func TestMigrateSearchSegmentCheckKeepsHealthyIndexes(t *testing.T) {
	const automergeMarker = "INSERT INTO messages_fts(messages_fts, rank) VALUES('automerge', 7)"
	setup := func(t *testing.T) (*MessageStore, string) {
		t.Helper()
		s := newTestStore(t)
		chat := "a@s.whatsapp.net"
		if err := s.StoreChat(chat, "A", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.migrateSearch(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s, chat
	}
	store := func(t *testing.T, s *MessageStore, chat, id, content string) {
		t.Helper()
		if err := s.StoreMessage(id, chat, "111", content, time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	automerge := func(t *testing.T, s *MessageStore) int {
		t.Helper()
		var v int
		if err := s.db.QueryRow("SELECT v FROM messages_fts_config WHERE k = 'automerge'").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := map[string]struct {
		prepare func(t *testing.T, s *MessageStore, chat string)
		marker  string
		matches int
	}{
		// Trigrams need 3 characters: the index has docsize rows and no segment at all
		"short, empty and NULL content only": {
			prepare: func(t *testing.T, s *MessageStore, chat string) {
				store(t, s, chat, "s1", "ab")
				store(t, s, chat, "s2", "")
				if _, err := s.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('s3', ?, '111', NULL, '2024-01-01', 0)`, chat); err != nil {
					t.Fatal(err)
				}
			},
			marker: automergeMarker,
		},
		"single-message writes": {
			prepare: func(t *testing.T, s *MessageStore, chat string) {
				for i := range 300 {
					store(t, s, chat, fmt.Sprintf("w%d", i), fmt.Sprintf("kingfisher %d", i))
				}
			},
			marker:  automergeMarker,
			matches: 300,
		},
		"every message deleted": {
			prepare: func(t *testing.T, s *MessageStore, chat string) {
				for i := range 50 {
					store(t, s, chat, fmt.Sprintf("w%d", i), fmt.Sprintf("kingfisher %d", i))
				}
				if _, err := s.db.Exec("DELETE FROM messages"); err != nil {
					t.Fatal(err)
				}
			},
			marker: automergeMarker,
		},
		// A rebuild recreates the table and resets automerge to its default 4, so 0 is the marker
		"partly completed incremental merge": {
			prepare: func(t *testing.T, s *MessageStore, chat string) {
				for _, q := range []string{
					"INSERT INTO messages_fts(messages_fts, rank) VALUES('pgsz', 64)",
					"INSERT INTO messages_fts(messages_fts, rank) VALUES('automerge', 0)",
				} {
					if _, err := s.db.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
				for i := range 300 {
					store(t, s, chat, fmt.Sprintf("w%d", i), fmt.Sprintf("kingfisher %d", i))
				}
				if _, err := s.db.Exec("INSERT INTO messages_fts(messages_fts, rank) VALUES('merge', 2)"); err != nil {
					t.Fatal(err)
				}
				var segments int
				if err := s.db.QueryRow("SELECT count(DISTINCT id >> 37) FROM messages_fts_data WHERE id >= (1 << 37)").Scan(&segments); err != nil || segments < 2 {
					t.Fatalf("segments = %d, %v: the merge must be unfinished", segments, err)
				}
			},
			matches: 300,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, chat := setup(t)
			tc.prepare(t, s, chat)
			want := 0
			if tc.marker != "" {
				if _, err := s.db.Exec(tc.marker); err != nil {
					t.Fatal(err)
				}
				want = 7
			}
			s.db.SetMaxIdleConns(0)
			if err := s.migrateSearch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !s.ftsCheck.ran || s.ftsCheck.err != nil {
				t.Fatalf("ftsCheck = %+v, want a passed check that ran", s.ftsCheck)
			}
			if got := automerge(t, s); got != want {
				t.Fatalf("automerge = %d, want %d: the healthy index was rebuilt", got, want)
			}
			if tc.matches > 0 {
				if got := ftsHits(t, s, "kingfisher"); len(got) != tc.matches {
					t.Fatalf("MATCH kingfisher = %d hits, want %d", len(got), tc.matches)
				}
			}
		})
	}
}

// messages_fts gone but its triggers left behind make every write fail. They are dropped before
// anything that can fail, even when the migration is cancelled before it starts.
func TestMigrateSearchDropsOrphanedTriggersFirst(t *testing.T) {
	s := newTestStore(t)
	chat := "a@s.whatsapp.net"
	if err := s.StoreChat(chat, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("m1", chat, "111", "pelican", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE messages_fts"); err != nil {
		t.Fatal(err)
	}
	if n := ftsTriggers(t, s); n != 3 {
		t.Fatalf("setup: triggers = %d, want 3 left behind", n)
	}
	if err := s.StoreMessage("setup", chat, "111", "heron", time.Now(), false, "", "", "", nil, nil, nil, 0); err == nil {
		t.Fatal("setup: a write through the orphaned triggers must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.migrateSearch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := ftsTriggers(t, s); n != 0 {
		t.Fatalf("triggers = %d after a cancelled migration, want 0", n)
	}
	checkWritesWork(t, s, chat)
	// The next start builds the index over everything written meanwhile
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := ftsTriggers(t, s); n != 3 {
		t.Fatalf("triggers = %d after the retry, want 3", n)
	}
	if got := ftsHits(t, s, "cormorant"); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("MATCH cormorant = %v", got)
	}
}

// TestBundledSQLiteVersionFloor records the SQLite release the FTS repair
// tests were verified on (3.54.1). It is a floor, not an equality, so a later
// go-sqlite3 bump does not fail it.
func TestBundledSQLiteVersionFloor(t *testing.T) {
	version, number, _ := sqlite3.Version()
	if number < 3054001 {
		t.Fatalf("bundled SQLite %s (%d) is older than 3.54.1", version, number)
	}
}

// The statements of the README entry "Search index cannot be removed". Keep them in step with it.
var documentedFTSRecovery = []string{
	`DROP TRIGGER IF EXISTS messages_fts_insert`,
	`DROP TRIGGER IF EXISTS messages_fts_delete`,
	`DROP TRIGGER IF EXISTS messages_fts_update`,
	`PRAGMA writable_schema=ON`,
	`DELETE FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts' AND rootpage = 0`,
	`PRAGMA writable_schema=RESET`,
	`DROP TABLE IF EXISTS messages_fts_data`,
	`DROP TABLE IF EXISTS messages_fts_idx`,
	`DROP TABLE IF EXISTS messages_fts_docsize`,
	`DROP TABLE IF EXISTS messages_fts_config`,
}

// The README recovery removes a table SQLite cannot drop, and the next migration builds a
// working index.
func TestDocumentedFTSRecoveryRemovesBrokenTable(t *testing.T) {
	s := newTestStore(t)
	chat := "a@s.whatsapp.net"
	if err := s.StoreChat(chat, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("m1", chat, "111", "pelican", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE messages_fts_config"); err != nil {
		t.Fatal(err)
	}
	// Fresh connections, like a restart
	s.db.SetMaxIdleConns(0)
	if err := s.migrateSearch(context.Background()); !errors.Is(err, errFTSNotDroppable) {
		t.Fatalf("err = %v, want errFTSNotDroppable", err)
	}
	// writable_schema is per connection, so every statement runs on one
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range documentedFTSRecovery {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var check string
	if err := conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateSearch(ctx); err != nil {
		t.Fatalf("migrateSearch after recovery: %v", err)
	}
	if got := ftsHits(t, s, "pelican"); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("hits = %v, want [m1]", got)
	}
	if n := ftsTriggers(t, s); n != 3 {
		t.Fatalf("triggers = %d, want 3", n)
	}
}

// The log line names a README section; it must not point at one that was renamed.
func TestFTSRecoverySectionExistsInREADME(t *testing.T) {
	b, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), ftsRecoverySection) {
		t.Fatalf("README.md has no %q section", ftsRecoverySection)
	}
}
