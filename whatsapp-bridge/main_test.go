package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// captureStderr runs fn with os.Stderr redirected and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = wr
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(rd)
		out <- string(b)
	}()
	defer func() { os.Stderr = orig }()
	fn()
	wr.Close()
	os.Stderr = orig
	return <-out
}

const (
	secretBody = "SECRET-BODY-4711"
	secretFile = "secret-file-4711.pdf"
)

func secretMessages() []*waE2E.Message {
	return []*waE2E.Message{
		{Conversation: proto.String(secretBody)},
		{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String(secretFile), Caption: proto.String(secretBody)}},
	}
}

func assertNoSecrets(t *testing.T, logs string) {
	t.Helper()
	if strings.Contains(logs, secretBody) || strings.Contains(logs, secretFile) {
		t.Fatalf("message body or file name leaked into logs:\n%s", logs)
	}
}

func TestLiveMessageLogsNoBody(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	logger := newStderrLogger("Test", "DEBUG")
	logs := captureStderr(t, func() {
		for i, m := range secretMessages() {
			evt := &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: pnJID(testPN), Sender: pnJID(testPN)},
					ID:            types.MessageID("live" + string(rune('a'+i))),
					Timestamp:     time.Now(),
				},
				Message: m,
			}
			handleMessage(r, ms, evt, logger)
		}
	})
	if countMsgs(t, ms, pnJID(testPN).String()) != 2 {
		t.Fatalf("messages not stored; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "text bytes") {
		t.Fatalf("expected a metadata log line, got:\n%s", logs)
	}
	assertNoSecrets(t, logs)
}

func TestHistorySyncLogsNoBody(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	logger := newStderrLogger("Test", "DEBUG")
	ts := uint64(time.Now().Unix())
	var msgs []*waHistorySync.HistorySyncMsg
	for i, m := range secretMessages() {
		msgs = append(msgs, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(pnJID(testPN).String()), FromMe: proto.Bool(false), ID: proto.String("hist" + string(rune('a'+i)))},
			Message:          m,
			MessageTimestamp: proto.Uint64(ts),
		}})
	}
	hs := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{
		{ID: proto.String(pnJID(testPN).String()), Messages: msgs},
	}}}
	logs := captureStderr(t, func() {
		handleHistorySync(r, testMyPN, ms, hs, logger)
	})
	if !strings.Contains(logs, "Stored message") {
		t.Fatalf("expected a metadata log line, got:\n%s", logs)
	}
	assertNoSecrets(t, logs)
}

// A DM must be stored while holding the repair lock: a repair that merges (deletes) the
// LID chat between StoreChat and StoreMessage would make the insert fail and drop it.
func TestMessageStoreWaitsForRepair(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	logger := newStderrLogger("Test", "ERROR")
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: lidJID(testLID), Sender: lidJID(testLID)},
			ID:            "m1",
			Timestamp:     time.Now(),
		},
		Message: &waE2E.Message{Conversation: proto.String("hi")},
	}
	hs := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID: proto.String(lidJID(testLID2).String()),
		Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(lidJID(testLID2).String()), ID: proto.String("h1")},
			Message:          &waE2E.Message{Conversation: proto.String("hi")},
			MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
		}}},
	}}}}

	r.runMu.Lock() // a repair pass is running
	done := make(chan struct{}, 2)
	go func() { handleMessage(r, ms, evt, logger); done <- struct{}{} }()
	go func() { handleHistorySync(r, testMyPN, ms, hs, logger); done <- struct{}{} }()
	time.Sleep(50 * time.Millisecond)
	stored := countMsgs(t, ms, lidJID(testLID).String()) + countMsgs(t, ms, lidJID(testLID2).String())
	r.runMu.Unlock()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("handler did not finish after the repair released the lock")
		}
	}
	if stored != 0 {
		t.Fatalf("%d message(s) stored while a repair held the lock", stored)
	}
	if countMsgs(t, ms, lidJID(testLID).String()) != 1 || countMsgs(t, ms, lidJID(testLID2).String()) != 1 {
		t.Fatal("messages not stored after the repair finished")
	}

	// Group messages never wait for the repair lock.
	r.runMu.Lock()
	defer r.runMu.Unlock()
	group := types.NewJID("120363000000000001", types.GroupServer)
	gEvt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: group, Sender: pnJID(testPN), IsGroup: true},
			ID:            "g1",
			Timestamp:     time.Now(),
		},
		Message: &waE2E.Message{Conversation: proto.String("hi")},
	}
	go func() { handleMessage(r, ms, gEvt, logger); done <- struct{}{} }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("group message blocked on the repair lock")
	}
}

// A history sync resolving an unnamed group over the network must not hold the repair lock,
// or live DMs (which take it) would stall behind the lookup.
func TestHistorySyncGroupLookupDoesNotBlockLiveDM(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	gi := func(ctx context.Context, jid types.JID) (string, error) {
		close(entered)
		<-release
		return "Slow Group", nil
	}
	r, ms := newTestResolver(t, nil, newFakeLIDs(), gi)
	logger := newStderrLogger("Test", "ERROR")
	group := types.NewJID("120363000000000009", types.GroupServer)
	hs := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID: proto.String(group.String()),
		Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(group.String()), ID: proto.String("hg1")},
			Message:          &waE2E.Message{Conversation: proto.String("hi")},
			MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
		}}},
	}}}}
	syncDone := make(chan struct{})
	go func() { handleHistorySync(r, testMyPN, ms, hs, logger); close(syncDone) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("group name lookup never started")
	}

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: pnJID(testPN), Sender: pnJID(testPN)},
			ID:            "live1",
			Timestamp:     time.Now(),
		},
		Message: &waE2E.Message{Conversation: proto.String("hi")},
	}
	dmDone := make(chan struct{})
	go func() { handleMessage(r, ms, evt, logger); close(dmDone) }()
	select {
	case <-dmDone:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("live DM blocked behind a history-sync group name lookup")
	}
	close(release)
	select {
	case <-syncDone:
	case <-time.After(3 * time.Second):
		t.Fatal("history sync did not finish")
	}
	if countMsgs(t, ms, group.String()) != 1 || countMsgs(t, ms, pnJID(testPN).String()) != 1 {
		t.Fatal("messages not stored")
	}
}
