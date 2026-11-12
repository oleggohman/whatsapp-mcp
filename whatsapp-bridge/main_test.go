package main

import (
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
