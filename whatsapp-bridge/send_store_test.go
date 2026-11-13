package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const testSelfPN = "15550199999"

// storeSentLike wires a recorder handler the way run() does, with a test resolver.
func storeSentLike(r *chatResolver, ms *MessageStore, self types.JID) func(*sentMessage) {
	return func(sent *sentMessage) {
		handleMessage(r, ms, sentEvent(self, sent), newStderrLogger("T", "ERROR"))
	}
}

func sentRow(t *testing.T, ms *MessageStore, id string) (chat, sender, content string, fromMe bool) {
	t.Helper()
	err := ms.db.QueryRow("SELECT chat_jid, sender, content, is_from_me FROM messages WHERE id = ?", id).
		Scan(&chat, &sender, &content, &fromMe)
	if err != nil {
		t.Fatalf("message %s not stored: %v", id, err)
	}
	return
}

func TestHandleSendStoresTheSentMessage(t *testing.T) {
	ts := time.Date(2026, 11, 13, 8, 6, 0, 0, time.UTC)
	// The send response reports our LID as sender, as whatsmeow does for PN recipients.
	fc := &fakeClient{connected: true, sendResp: whatsmeow.SendResponse{ID: "MSG1", Timestamp: ts, Sender: lidJID(testLID)}}
	a, h := newTestAPI(t, fc, "")
	r := newChatResolver(a.messageStore, &fakeContacts{}, newFakeLIDs(), nil)
	a.sent.register(fc, storeSentLike(r, a.messageStore, pnJID(testSelfPN)))
	if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"15551234","message":"hello there"}`); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	chat, sender, content, fromMe := sentRow(t, a.messageStore, "MSG1")
	if chat != "15551234@s.whatsapp.net" || sender != testSelfPN || content != "hello there" || !fromMe {
		t.Fatalf("chat=%q sender=%q content=%q fromMe=%v", chat, sender, content, fromMe)
	}
	chats, err := a.messageStore.GetChats()
	if err != nil || !chats["15551234@s.whatsapp.net"].Equal(ts) {
		t.Fatalf("last_message_time = %v, err = %v", chats["15551234@s.whatsapp.net"], err)
	}
}

func TestSentMessageToLIDIsStoredUnderThePhoneNumberChat(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(testLID, testPN), nil)
	fc := &fakeClient{connected: true, sendResp: whatsmeow.SendResponse{ID: "MSG2", Timestamp: time.Now()}}
	ok, _, sent := sendAndDescribe(fc, lidJID(testLID).String(), "hi", "")
	if !ok {
		t.Fatal("send failed")
	}
	storeSentLike(r, ms, pnJID(testSelfPN))(sent)
	chat, sender, _, _ := sentRow(t, ms, "MSG2")
	if chat != pnJID(testPN).String() || sender != testSelfPN {
		t.Fatalf("chat=%q sender=%q", chat, sender)
	}
}

func TestSentGroupMessageKeepsTheGroupChat(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), func(_ context.Context, _ types.JID) (string, error) { return "Family", nil })
	fc := &fakeClient{connected: true, sendResp: whatsmeow.SendResponse{ID: "MSG3", Timestamp: time.Now()}}
	_, _, sent := sendAndDescribe(fc, "120363@g.us", "hi all", "")
	storeSentLike(r, ms, pnJID(testSelfPN))(sent)
	if chat, sender, _, _ := sentRow(t, ms, "MSG3"); chat != "120363@g.us" || sender != testSelfPN {
		t.Fatalf("chat=%q sender=%q", chat, sender)
	}
	if name := ms.getChatName("120363@g.us"); name != "Family" {
		t.Fatalf("group name = %q", name)
	}
}

func TestHandleSendStoresMediaInfo(t *testing.T) {
	fc := &fakeClient{connected: true, sendResp: whatsmeow.SendResponse{ID: "MSG4", Timestamp: time.Now()}}
	a, h := newTestAPI(t, fc, "")
	r := newChatResolver(a.messageStore, &fakeContacts{}, newFakeLIDs(), nil)
	a.sent.register(fc, storeSentLike(r, a.messageStore, pnJID(testSelfPN)))
	img := writeTemp(t, "pic.png", []byte("\x89PNG fake"))
	if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"1555","message":"look","media_path":"`+img+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var mediaType, url string
	if err := a.messageStore.db.QueryRow("SELECT media_type, url FROM messages WHERE id = 'MSG4'").Scan(&mediaType, &url); err != nil ||
		mediaType != "image" || url != "u" {
		t.Fatalf("media_type=%q url=%q err=%v", mediaType, url, err)
	}
}

func TestHandleSendWithoutAStoreHandlerStillSucceeds(t *testing.T) {
	fc := &fakeClient{connected: true, sendResp: whatsmeow.SendResponse{ID: "MSG5"}}
	a, h := newTestAPI(t, fc, "")
	logs := captureStderr(t, func() {
		if rec := do(h, http.MethodPost, "/api/send", `{"recipient":"1555","message":"hi"}`); rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	var n int
	if err := a.messageStore.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 0 {
		t.Fatalf("messages = %d, err = %v", n, err)
	}
	if !strings.Contains(logs, "Sent message not stored") {
		t.Fatalf("logs = %q", logs)
	}
}

func TestHandleSendDoesNotStoreFailedSends(t *testing.T) {
	fc := &fakeClient{connected: false}
	a, h := newTestAPI(t, fc, "")
	called := false
	a.sent.register(fc, func(*sentMessage) { called = true })
	do(h, http.MethodPost, "/api/send", `{"recipient":"1555","message":"hi"}`)
	if called {
		t.Fatal("a failed send must not be stored")
	}
}

func TestSentRecorderIgnoresNil(t *testing.T) {
	var r sentRecorder
	fc := &fakeClient{}
	r.register(fc, func(*sentMessage) { t.Fatal("must not be called") })
	if r.record(fc, nil) {
		t.Fatal("nil sent message recorded")
	}
}

func TestSentEventAndSelfJID(t *testing.T) {
	sent := &sentMessage{
		chat: types.NewJID("120363", types.GroupServer),
		resp: whatsmeow.SendResponse{ID: "X", Sender: lidJID(testLID)},
		msg:  &waE2E.Message{Conversation: proto.String("hi")},
	}
	before := time.Now()
	ev := sentEvent(pnJID(testSelfPN), sent)
	if !ev.Info.IsFromMe || !ev.Info.IsGroup || ev.Info.ID != "X" || ev.Info.Timestamp.Before(before) {
		t.Fatalf("info = %+v", ev.Info)
	}
	if got := selfJID(&store.Device{}, sent); got != lidJID(testLID) {
		t.Fatalf("unpaired device falls back to the response sender, got %v", got)
	}
	id := types.JID{User: testSelfPN, Server: types.DefaultUserServer, Device: 7}
	if got := selfJID(&store.Device{ID: &id}, sent); got != pnJID(testSelfPN) {
		t.Fatalf("self = %v", got)
	}
}
