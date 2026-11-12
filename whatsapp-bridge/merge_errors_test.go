package main

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// hookLIDs runs onLookup on every GetPNForLID call, after delegating to the wrapped map.
type hookLIDs struct {
	*fakeLIDs
	onLookup func()
}

func (h *hookLIDs) GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error) {
	pn, err := h.fakeLIDs.GetPNForLID(ctx, lid)
	if h.onLookup != nil {
		h.onLookup()
	}
	return pn, err
}

// seedMergeable stores a LID chat and a PN chat that exercise every merge step: a message
// duplicated under both chats and a LID-addressed sender that has a mapping.
func seedMergeable(t *testing.T, ms *MessageStore) (lid, pn string) {
	t.Helper()
	lid, pn = lidJID(testLID).String(), pnJID(testPN).String()
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(lid, "Bob", t1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreChat(pn, "Alice", t1); err != nil {
		t.Fatal(err)
	}
	putMsg(t, ms, "dup", lid, testLID, "from-lid", t1)
	putMsg(t, ms, "dup", pn, testPN, "from-pn", t1)
	putMsg(t, ms, "only", lid, testLID, "only-lid", t1)
	return lid, pn
}

func TestMergeChatRollsBackWhenAStepFails(t *testing.T) {
	steps := map[string]string{
		"insert PN chat":   "CREATE TRIGGER x BEFORE INSERT ON chats BEGIN SELECT RAISE(ABORT, 'step-boom'); END",
		"move messages":    "CREATE TRIGGER x BEFORE UPDATE OF chat_jid ON messages BEGIN SELECT RAISE(ABORT, 'step-boom'); END",
		"delete leftovers": "CREATE TRIGGER x BEFORE DELETE ON messages BEGIN SELECT RAISE(ABORT, 'step-boom'); END",
		"map senders":      "CREATE TRIGGER x BEFORE UPDATE OF sender ON messages BEGIN SELECT RAISE(ABORT, 'step-boom'); END",
		"update PN chat":   "CREATE TRIGGER x BEFORE UPDATE ON chats BEGIN SELECT RAISE(ABORT, 'step-boom'); END",
		"delete LID chat":  "CREATE TRIGGER x BEFORE DELETE ON chats BEGIN SELECT RAISE(ABORT, 'step-boom'); END",
	}
	for name, ddl := range steps {
		t.Run(name, func(t *testing.T) {
			ms := newTestStore(t)
			lid, pn := seedMergeable(t, ms)
			if _, err := ms.db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
			err := ms.mergeChat(context.Background(), lid, pn, newFakeLIDs(testLID, testPN))
			if err == nil || !strings.Contains(err.Error(), "step-boom") {
				t.Fatalf("err = %v", err)
			}
			// Nothing of the failed merge may be visible
			if _, _, ok := chatRow(t, ms, lid); !ok {
				t.Error("LID chat lost by a rolled-back merge")
			}
			if got := countMsgs(t, ms, lid); got != 2 {
				t.Errorf("LID messages = %d, want 2", got)
			}
			if got := countMsgs(t, ms, pn); got != 1 {
				t.Errorf("PN messages = %d, want 1", got)
			}
		})
	}
}

func TestMergeChatInvalidInputs(t *testing.T) {
	ctx := context.Background()
	ms := newTestStore(t)
	pn := pnJID(testPN).String()
	if err := ms.mergeChat(ctx, "1.2.3@lid", pn, nil); err == nil || !strings.Contains(err.Error(), "parse lid jid") {
		t.Errorf("bad lid: %v", err)
	}
	if err := ms.mergeChat(ctx, lidJID(testLID).String(), "1.2.3@s.whatsapp.net", nil); err == nil || !strings.Contains(err.Error(), "parse pn jid") {
		t.Errorf("bad pn: %v", err)
	}
	closed := closedStore(t)
	if err := closed.mergeChat(ctx, lidJID(testLID).String(), pn, nil); err == nil {
		t.Error("closed DB must fail the sender query")
	}
}

func TestMergeChatSenderRowFailures(t *testing.T) {
	ctx := context.Background()
	lid, pn := lidJID(testLID).String(), pnJID(testPN).String()
	boom := errors.New("read boom")
	s := fakeStore(&fakeConnector{cols: []string{"a", "b"}, rows: [][]driver.Value{{"x", "y"}}})
	if err := s.mergeChat(ctx, lid, pn, nil); err == nil || !strings.Contains(err.Error(), "destination arguments") {
		t.Errorf("scan: %v", err)
	}
	s = fakeStore(&fakeConnector{cols: []string{"sender"}, nextErr: boom})
	if err := s.mergeChat(ctx, lid, pn, nil); !errors.Is(err, boom) {
		t.Errorf("iteration: %v", err)
	}
	// Senders resolve, then the write transaction cannot begin
	s = fakeStore(&fakeConnector{cols: []string{"sender"}, rows: [][]driver.Value{{"x"}}})
	if err := s.mergeChat(ctx, lid, pn, nil); err == nil || !strings.Contains(err.Error(), "begin boom") {
		t.Errorf("begin: %v", err)
	}
}

func TestMergeChatBeginFailsWhenContextEndsDuringLookups(t *testing.T) {
	ms := newTestStore(t)
	lid, pn := seedMergeable(t, ms)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lids := &hookLIDs{fakeLIDs: newFakeLIDs(testLID, testPN), onLookup: cancel}
	if err := ms.mergeChat(ctx, lid, pn, lids); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, _, ok := chatRow(t, ms, lid); !ok {
		t.Fatal("LID chat must survive")
	}
}

func TestMergeChatUnreadableTimestamps(t *testing.T) {
	ctx := context.Background()
	lid, pn := lidJID(testLID).String(), pnJID(testPN).String()
	t.Run("LID chat", func(t *testing.T) {
		ms := newTestStore(t)
		if _, err := ms.db.Exec("INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'x', x'00')", lid); err != nil {
			t.Fatal(err)
		}
		if err := ms.mergeChat(ctx, lid, pn, nil); err == nil {
			t.Fatal("expected a scan error")
		}
	})
	t.Run("PN chat", func(t *testing.T) {
		ms := newTestStore(t)
		if err := ms.StoreChat(lid, "x", time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := ms.db.Exec("INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'y', x'00')", pn); err != nil {
			t.Fatal(err)
		}
		if err := ms.mergeChat(ctx, lid, pn, nil); err == nil {
			t.Fatal("expected a scan error")
		}
		if _, _, ok := chatRow(t, ms, lid); !ok {
			t.Fatal("rolled back merge must keep the LID chat")
		}
	})
}

// With an empty PN name and only a placeholder LID name, the LID name is still better than nothing.
func TestMergeChatEmptyPNNameFallsBackToLIDName(t *testing.T) {
	ms := newTestStore(t)
	lid, pn := lidJID(testLID).String(), pnJID(testPN).String()
	if err := ms.StoreChat(lid, "LID "+testLID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreChat(pn, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ms.mergeChat(context.Background(), lid, pn, nil); err != nil {
		t.Fatal(err)
	}
	if name, _, _ := chatRow(t, ms, pn); name != "LID "+testLID {
		t.Fatalf("name = %q", name)
	}
}
