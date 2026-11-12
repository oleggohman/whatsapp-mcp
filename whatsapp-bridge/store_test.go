package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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
	want := "file:/x/messages.db?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate&_journal_mode=WAL"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNewMessageStoreUsesWAL(t *testing.T) {
	s := newTestStore(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
}
