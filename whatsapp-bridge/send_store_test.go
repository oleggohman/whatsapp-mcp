package main

import (
	"net/http"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func sentResp(id string, ts time.Time) whatsmeow.SendResponse {
	return whatsmeow.SendResponse{ID: id, Timestamp: ts, Sender: types.JID{User: "me", Server: "s.whatsapp.net"}}
}

func TestHandleSendStoresTheSentMessage(t *testing.T) {
	ts := time.Date(2026, 11, 13, 8, 6, 0, 0, time.UTC)
	fc := &fakeClient{connected: true, sendResp: sentResp("MSG1", ts)}
	a, h := newTestAPI(t, fc, "")
	if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"15551234","message":"hello there"}`); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var content, sender string
	var fromMe bool
	err := a.messageStore.db.QueryRow(
		"SELECT content, sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ?",
		"MSG1", "15551234@s.whatsapp.net").Scan(&content, &sender, &fromMe)
	if err != nil || content != "hello there" || sender != "me" || !fromMe {
		t.Fatalf("row: content=%q sender=%q fromMe=%v err=%v", content, sender, fromMe, err)
	}
	if name := a.messageStore.getChatName("15551234@s.whatsapp.net"); name != "15551234" {
		t.Fatalf("new chat name = %q", name)
	}
}

func TestHandleSendKeepsKnownChatNameAndUpdatesLastTime(t *testing.T) {
	ts := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	fc := &fakeClient{connected: true, sendResp: sentResp("MSG2", ts)}
	a, h := newTestAPI(t, fc, "")
	jid := "120363@g.us"
	if err := a.messageStore.StoreChat(jid, "Family", ts.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"120363@g.us","message":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if name := a.messageStore.getChatName(jid); name != "Family" {
		t.Fatalf("chat name overwritten: %q", name)
	}
	chats, err := a.messageStore.GetChats()
	if err != nil || !chats[jid].Equal(ts) {
		t.Fatalf("last_message_time = %v, err = %v", chats[jid], err)
	}
}

func TestHandleSendStoresMediaInfo(t *testing.T) {
	fc := &fakeClient{connected: true, sendResp: sentResp("MSG3", time.Now())}
	a, h := newTestAPI(t, fc, "")
	img := writeTemp(t, "pic.png", []byte("\x89PNG fake"))
	if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"1555","message":"look","media_path":"`+img+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var mediaType, url, content string
	err := a.messageStore.db.QueryRow(
		"SELECT media_type, url, content FROM messages WHERE id = 'MSG3'").Scan(&mediaType, &url, &content)
	if err != nil || mediaType != "image" || url != "u" { // captions are not content, as for received media
		t.Fatalf("media_type=%q url=%q content=%q err=%v", mediaType, url, content, err)
	}
}

func TestHandleSendDoesNotStoreFailedSends(t *testing.T) {
	fc := &fakeClient{connected: false}
	a, h := newTestAPI(t, fc, "")
	do(h, http.MethodPost, "/api/send", `{"recipient":"1555","message":"hi"}`)
	var n int
	if err := a.messageStore.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 0 {
		t.Fatalf("messages = %d, err = %v", n, err)
	}
}

func TestHandleSendStillSucceedsWhenStoringFails(t *testing.T) {
	fc := &fakeClient{connected: true, sendResp: sentResp("MSG4", time.Now())}
	a, h := newTestAPI(t, fc, "")
	a.messageStore.db.Close()
	if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"1555","message":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("a storage failure must not fail the send: code=%d", rec.Code)
	}
}

func TestRecordSentEdgeCases(t *testing.T) {
	if err := recordSent(nil, &sentMessage{}); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t)
	if err := recordSent(s, nil); err != nil {
		t.Fatal(err)
	}
	// A zero server timestamp falls back to now.
	fc := &fakeClient{connected: true, sendResp: whatsmeow.SendResponse{ID: "MSG5"}}
	ok, _, sent := sendAndDescribe(fc, "1555", "hi", "")
	if !ok {
		t.Fatal("send failed")
	}
	before := time.Now()
	if err := recordSent(s, sent); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.GetMessages("1555@s.whatsapp.net", 1)
	if err != nil || len(msgs) != 1 || msgs[0].Time.Before(before.Add(-time.Second)) {
		t.Fatalf("msgs=%v err=%v", msgs, err)
	}
}
