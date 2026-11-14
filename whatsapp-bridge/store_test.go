package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err := s.StoreMessage("m1", "a@s.whatsapp.net", "111", "old row about pelican", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'messages_fts'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("messages_fts in sqlite_master = %d, %v", n, err)
	}
	if got := ftsHits(t, s, "pelican"); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("MATCH = %v", got)
	}
	// An index that diverged from messages (a crashed process) is rebuilt on the next start
	if _, err := s.db.Exec("INSERT INTO messages_fts(messages_fts) VALUES('delete-all')"); err != nil {
		t.Fatal(err)
	}
	if indexed, total := ftsCounts(t, s); indexed != 0 || total != 1 {
		t.Fatalf("after delete-all: docsize = %d, messages = %d", indexed, total)
	}
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if indexed, total := ftsCounts(t, s); indexed != 1 || total != 1 {
		t.Fatalf("after rebuild: docsize = %d, messages = %d", indexed, total)
	}
}

func TestMigrateSearchRestoresDroppedTriggers(t *testing.T) {
	s := newTestStore(t)
	if err := s.migrateSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	names := []string{"messages_fts_insert", "messages_fts_delete", "messages_fts_update"}
	for _, n := range names {
		if _, err := s.db.Exec("DROP TRIGGER " + n); err != nil {
			t.Fatal(err)
		}
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
}
