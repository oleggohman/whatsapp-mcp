package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waVnameCert"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func liveEvent(chat, sender types.JID, id string, msg *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender},
			ID:            types.MessageID(id),
			Timestamp:     time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		},
		Message: msg,
	}
}

func storedChatName(t *testing.T, ms *MessageStore, jid types.JID) string {
	t.Helper()
	name, _, ok := chatRow(t, ms, jid.String())
	if !ok {
		t.Fatalf("chat %s not stored", jid)
	}
	return name
}

func TestHandleMessageUsesVerifiedNameForIncomingDM(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	evt := liveEvent(pnJID(testPN), pnJID(testPN), "v1", &waE2E.Message{Conversation: proto.String("hi")})
	evt.Info.VerifiedName = &types.VerifiedName{Details: &waVnameCert.VerifiedNameCertificate_Details{VerifiedName: proto.String("Acme Corp")}}
	evt.Info.PushName = "push"
	captureStderr(t, func() { handleMessage(r, ms, evt, newStderrLogger("T", "ERROR")) })
	if got := storedChatName(t, ms, pnJID(testPN)); got != "Acme Corp" {
		t.Fatalf("name = %q, want the verified name", got)
	}
}

func TestHandleMessageIgnoresNameHintsOfOutgoingMessages(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	evt := liveEvent(pnJID(testPN), pnJID(testMyPN), "o1", &waE2E.Message{Conversation: proto.String("hi")})
	evt.Info.IsFromMe = true
	evt.Info.PushName = "my own push name"
	logs := captureStderr(t, func() { handleMessage(r, ms, evt, newStderrLogger("T", "ERROR")) })
	if got := storedChatName(t, ms, pnJID(testPN)); got != "+"+testPN {
		t.Fatalf("name = %q, must not take the sender's push name", got)
	}
	if !strings.Contains(logs, "→") {
		t.Fatalf("outgoing messages are logged with an arrow: %q", logs)
	}
}

func TestHandleMessageWithoutContentOnlyTouchesChat(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	evt := liveEvent(pnJID(testPN), pnJID(testPN), "e1", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{}})
	captureStderr(t, func() { handleMessage(r, ms, evt, newStderrLogger("T", "ERROR")) })
	if countMsgs(t, ms, pnJID(testPN).String()) != 0 {
		t.Fatal("a message without text or media must not be stored")
	}
	if _, last, ok := chatRow(t, ms, pnJID(testPN).String()); !ok || !last.Equal(evt.Info.Timestamp) {
		t.Fatalf("chat timestamp not updated: %v %v", last, ok)
	}
}

func TestHandleMessageLogsStoreFailures(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	ms.Close()
	evt := liveEvent(pnJID(testPN), pnJID(testPN), "f1", &waE2E.Message{Conversation: proto.String("hi")})
	logs := captureStderr(t, func() { handleMessage(r, ms, evt, newStderrLogger("T", "WARN")) })
	for _, want := range []string{"Failed to store chat:", "Failed to store message:"} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in %q", want, logs)
		}
	}
	if strings.Contains(logs, "text bytes") {
		t.Errorf("a failed store must not be logged as received: %q", logs)
	}
}

func TestSelfUser(t *testing.T) {
	dev := &store.Device{}
	if got := selfUser(dev); got != "" {
		t.Fatalf("unpaired self = %q", got)
	}
	id := types.NewADJID(testMyPN, 0, 7)
	dev.ID = &id
	if got := selfUser(dev); got != testMyPN {
		t.Fatalf("self = %q", got)
	}
}

func TestConversationName(t *testing.T) {
	cases := []struct {
		conv *waHistorySync.Conversation
		want string
	}{
		{&waHistorySync.Conversation{Name: proto.String("Name"), DisplayName: proto.String("Display")}, "Name"},
		{&waHistorySync.Conversation{DisplayName: proto.String("Display")}, "Display"},
		{&waHistorySync.Conversation{}, ""},
	}
	for _, tc := range cases {
		if got := conversationName(tc.conv); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// histMsg builds a history-sync message for chat with optional pieces.
func histMsg(chat, id string, ts uint64, m *waE2E.Message) *waHistorySync.HistorySyncMsg {
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{RemoteJID: proto.String(chat), ID: proto.String(id)},
		Message:          m,
		MessageTimestamp: proto.Uint64(ts),
	}}
}

func text(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func TestSyncConversationSkipsUnusableConversations(t *testing.T) {
	ctx := context.Background()
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	logger := newStderrLogger("T", "WARN")
	ts := uint64(time.Now().Unix())
	chat := pnJID(testPN).String()

	t.Run("no ID", func(t *testing.T) {
		if n := syncConversation(ctx, r, "me", ms, &waHistorySync.Conversation{}, logger); n != 0 {
			t.Fatalf("n = %d", n)
		}
	})
	t.Run("unparseable ID", func(t *testing.T) {
		var n int
		logs := captureStderr(t, func() {
			n = syncConversation(ctx, r, "me", ms, &waHistorySync.Conversation{ID: proto.String("1.2.3@s.whatsapp.net")}, logger)
		})
		if n != 0 || !strings.Contains(logs, "Failed to parse JID 1.2.3@s.whatsapp.net") {
			t.Fatalf("n=%d logs=%q", n, logs)
		}
	})
	t.Run("latest message missing", func(t *testing.T) {
		for _, m := range []*waHistorySync.HistorySyncMsg{nil, {}} {
			conv := &waHistorySync.Conversation{ID: proto.String(chat), Messages: []*waHistorySync.HistorySyncMsg{m, histMsg(chat, "x", ts, text("hi"))}}
			if n := syncConversation(ctx, r, "me", ms, conv, logger); n != 0 {
				t.Fatalf("n = %d", n)
			}
		}
		if countMsgs(t, ms, chat) != 0 {
			t.Fatal("nothing may be stored")
		}
	})
	t.Run("latest message without timestamp", func(t *testing.T) {
		conv := &waHistorySync.Conversation{ID: proto.String(chat), Messages: []*waHistorySync.HistorySyncMsg{histMsg(chat, "x", 0, text("hi"))}}
		if n := syncConversation(ctx, r, "me", ms, conv, logger); n != 0 || countMsgs(t, ms, chat) != 0 {
			t.Fatalf("n = %d", n)
		}
	})
}

func TestSyncConversationStoresOnlyUsableMessages(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs(testLID, testPN2)
	r, ms := newTestResolver(t, nil, lids, nil)
	logger := newStderrLogger("T", "WARN")
	group := types.NewJID("120363000000000042", types.GroupServer)
	ts := uint64(time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC).Unix())

	fromMe := histMsg(group.String(), "mine", ts, text("sent by me"))
	fromMe.Message.Key.FromMe = proto.Bool(true)
	viaLID := histMsg(group.String(), "lid-sender", ts, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("extended")}})
	viaLID.Message.Key.Participant = proto.String(lidJID(testLID).String())
	noKey := histMsg(group.String(), "", ts, text("keyless"))
	noKey.Message.Key = nil

	conv := &waHistorySync.Conversation{
		ID:          proto.String(group.String()),
		DisplayName: proto.String("The Group"),
		Messages: []*waHistorySync.HistorySyncMsg{
			fromMe, // latest message: sets the chat time
			viaLID,
			nil, // skipped
			{},  // skipped: no message info
			histMsg(group.String(), "empty", ts, &waE2E.Message{}), // no text, no media
			histMsg(group.String(), "nopayload", ts, nil),          // nil inner message
			histMsg(group.String(), "notime", 0, text("no timestamp")),
			noKey,
			histMsg(group.String(), "media", ts, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: proto.String("https://mmg.whatsapp.net/v/i.enc")}}),
		},
	}
	var n int
	captureStderr(t, func() { n = syncConversation(ctx, r, testMyPN, ms, conv, logger) })
	// mine, lid-sender, keyless (empty id) and media are usable
	if n != 4 {
		t.Fatalf("stored %d messages, want 4", n)
	}
	if got := storedChatName(t, ms, group); got != "The Group" {
		t.Fatalf("chat name = %q", got)
	}
	senders := map[string]string{}
	rows, err := ms.db.Query("SELECT id, sender, content, is_from_me FROM messages WHERE chat_jid = ?", group.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, sender, content string
		var mine bool
		if err := rows.Scan(&id, &sender, &content, &mine); err != nil {
			t.Fatal(err)
		}
		senders[id] = sender
		if id == "mine" && (!mine || content != "sent by me") {
			t.Errorf("own message stored wrong: mine=%v content=%q", mine, content)
		}
		if id == "lid-sender" && content != "extended" {
			t.Errorf("extended text content = %q", content)
		}
	}
	if senders["mine"] != testMyPN {
		t.Errorf("own sender = %q, want %q", senders["mine"], testMyPN)
	}
	if senders["lid-sender"] != testPN2 {
		t.Errorf("LID participant not mapped to its phone number: %q", senders["lid-sender"])
	}
	if senders[""] != group.User {
		t.Errorf("keyless sender = %q", senders[""])
	}
}

func TestSyncConversationNormalizesAndMergesLIDChats(t *testing.T) {
	ctx := context.Background()
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil) // no stored mapping
	logger := newStderrLogger("T", "WARN")
	ts := uint64(time.Now().Unix())
	lid, pn := lidJID(testLID), pnJID(testPN)

	// A LID chat stored earlier, before its phone number was known
	if err := ms.StoreChat(lid.String(), "LID "+testLID, time.Now()); err != nil {
		t.Fatal(err)
	}
	putMsg(t, ms, "old", lid.String(), testLID, "earlier", time.Now())

	// The conversation carries the PN itself, so no mapping is needed to normalize
	conv := &waHistorySync.Conversation{
		ID:       proto.String(lid.String()),
		PnJID:    proto.String(pn.String()),
		Messages: []*waHistorySync.HistorySyncMsg{histMsg(lid.String(), "new", ts, text("later"))},
	}
	var n int
	captureStderr(t, func() { n = syncConversation(ctx, r, "me", ms, conv, logger) })
	if n != 1 {
		t.Fatalf("n = %d", n)
	}
	if _, _, ok := chatRow(t, ms, lid.String()); ok {
		t.Fatal("LID chat must be merged into the PN chat")
	}
	if got := countMsgs(t, ms, pn.String()); got != 2 {
		t.Fatalf("PN chat has %d messages, want 2", got)
	}
}

func TestSyncConversationMergesSeparateLIDJIDField(t *testing.T) {
	ctx := context.Background()
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	logger := newStderrLogger("T", "WARN")
	lid, pn := lidJID(testLID), pnJID(testPN)
	if err := ms.StoreChat(lid.String(), "Bob", time.Now()); err != nil {
		t.Fatal(err)
	}
	putMsg(t, ms, "old", lid.String(), testLID, "earlier", time.Now())

	// A PN conversation that names its LID twin
	conv := &waHistorySync.Conversation{
		ID:       proto.String(pn.String()),
		LidJID:   proto.String(lid.String()),
		Messages: []*waHistorySync.HistorySyncMsg{histMsg(pn.String(), "new", uint64(time.Now().Unix()), text("later"))},
	}
	captureStderr(t, func() { syncConversation(ctx, r, "me", ms, conv, logger) })
	if _, _, ok := chatRow(t, ms, lid.String()); ok {
		t.Fatal("LID twin must be merged")
	}
	if got := countMsgs(t, ms, pn.String()); got != 2 {
		t.Fatalf("PN chat has %d messages, want 2", got)
	}
	// An unparseable twin is ignored
	conv.LidJID = proto.String("1.2.3@lid")
	captureStderr(t, func() { syncConversation(ctx, r, "me", ms, conv, logger) })
}

func TestSyncConversationLogsStoreFailure(t *testing.T) {
	ctx := context.Background()
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	if _, err := ms.db.Exec("CREATE TRIGGER x BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'insert-boom'); END"); err != nil {
		t.Fatal(err)
	}
	chat := pnJID(testPN).String()
	conv := &waHistorySync.Conversation{ID: proto.String(chat), Messages: []*waHistorySync.HistorySyncMsg{histMsg(chat, "a", uint64(time.Now().Unix()), text("hi"))}}
	var n int
	logs := captureStderr(t, func() { n = syncConversation(ctx, r, "me", ms, conv, newStderrLogger("T", "WARN")) })
	if n != 0 || !strings.Contains(logs, "Failed to store history message:") || !strings.Contains(logs, "insert-boom") {
		t.Fatalf("n=%d logs=%q", n, logs)
	}
}

// fakeHistory is a historyClient with switchable behavior.
type fakeHistory struct {
	connected bool
	paired    bool
	request   *waE2E.Message
	sendErr   error
	sent      []types.JID
	count     int
}

func (f *fakeHistory) IsConnected() bool { return f.connected }
func (f *fakeHistory) Paired() bool      { return f.paired }
func (f *fakeHistory) BuildHistorySyncRequest(_ *types.MessageInfo, count int) *waE2E.Message {
	f.count = count
	return f.request
}
func (f *fakeHistory) SendMessage(_ context.Context, to types.JID, _ *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sent = append(f.sent, to)
	return whatsmeow.SendResponse{}, f.sendErr
}

func TestRequestHistorySync(t *testing.T) {
	cases := []struct {
		name     string
		client   historyClient
		want     string
		wantSent bool
	}{
		{"nil client", nil, "Client is not initialized", false},
		{"not connected", &fakeHistory{paired: true}, "Client is not connected", false},
		{"not logged in", &fakeHistory{connected: true}, "Client is not logged in", false},
		{"build fails", &fakeHistory{connected: true, paired: true}, "Failed to build history sync request", false},
		{"send fails", &fakeHistory{connected: true, paired: true, request: &waE2E.Message{}, sendErr: errors.New("net down")}, "Failed to request history sync: net down", true},
		{"success", &fakeHistory{connected: true, paired: true, request: &waE2E.Message{}}, "History sync requested", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureStderr(t, func() { requestHistorySync(tc.client) })
			if !strings.Contains(logs, tc.want) {
				t.Fatalf("log = %q, want %q", logs, tc.want)
			}
			if f, ok := tc.client.(*fakeHistory); ok {
				if (len(f.sent) == 1) != tc.wantSent {
					t.Fatalf("sent = %v", f.sent)
				}
				if tc.wantSent && (f.sent[0] != types.JID{User: "status", Server: "s.whatsapp.net"} || f.count != 100) {
					t.Fatalf("sent to %v with count %d", f.sent[0], f.count)
				}
			}
		})
	}
}
