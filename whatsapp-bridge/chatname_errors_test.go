package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// errContacts fails every lookup except for the users in ok.
type errContacts struct {
	store.ContactStore
	ok map[string]types.ContactInfo
}

func (e *errContacts) GetContact(_ context.Context, jid types.JID) (types.ContactInfo, error) {
	if c, found := e.ok[jid.User]; found {
		return c, nil
	}
	return types.ContactInfo{}, errors.New("contact store unavailable")
}

func TestContactForSkipsFailingLookups(t *testing.T) {
	ms := newTestStore(t)
	// The PN record cannot be read; the LID record still supplies the name.
	contacts := &errContacts{ok: map[string]types.ContactInfo{testLID: {PushName: "Bob"}}}
	r := newChatResolver(ms, contacts, newFakeLIDs(testLID, testPN), nil)
	if got := r.contactFor(context.Background(), pnJID(testPN)); got.PushName != "Bob" || got.FullName != "" {
		t.Fatalf("contact = %+v", got)
	}
	if got := r.resolveName(context.Background(), pnJID(testPN), nameHints{}); got != "Bob" {
		t.Fatalf("name = %q", got)
	}
}

func TestMergeIfMappedSkipsMissingLIDChat(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(testLID, testPN), nil)
	pn := pnJID(testPN)
	out := captureStderr(t, func() { r.mergeIfMapped(context.Background(), lidJID(testLID), pn) })
	if out != "" {
		t.Fatalf("unexpected log %q", out)
	}
	if _, _, ok := chatRow(t, ms, pn.String()); ok {
		t.Fatal("no PN chat may be created for a LID chat that does not exist")
	}
}

func TestMergeIfMappedLogsMergeFailure(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(testLID, testPN), nil)
	lid, _ := seedMergeable(t, ms)
	if _, err := ms.db.Exec("CREATE TRIGGER x BEFORE DELETE ON chats BEGIN SELECT RAISE(ABORT, 'step-boom'); END"); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() { r.mergeIfMapped(context.Background(), lidJID(testLID), pnJID(testPN)) })
	if !strings.Contains(out, "Failed to merge chat "+lid) || !strings.Contains(out, "step-boom") {
		t.Fatalf("log = %q", out)
	}
	if _, _, ok := chatRow(t, ms, lid); !ok {
		t.Fatal("LID chat must stay after a failed merge")
	}
}

func TestRepairErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("first listing fails", func(t *testing.T) {
		r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
		ms.Close()
		if err := r.repair(ctx); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("unmergeable LID chats are skipped", func(t *testing.T) {
		// Without a LID store, with an unparseable JID or with an unmapped LID nothing is merged
		for name, lids := range map[string]*fakeLIDs{"unmapped": newFakeLIDs()} {
			r, ms := newTestResolver(t, nil, lids, nil)
			lid := lidJID(testLID).String()
			if err := ms.StoreChat(lid, "Bob", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreChat("1.2.3@lid", "Broken", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := r.repair(ctx); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, _, ok := chatRow(t, ms, lid); !ok {
				t.Fatalf("%s: chat must stay", name)
			}
		}
		ms := newTestStore(t)
		r := newLazyChatResolver(ms, func() store.ContactStore { return nil }, func() store.LIDStore { return nil }, nil)
		if err := ms.StoreChat(lidJID(testLID).String(), "Bob", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.repair(ctx); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := chatRow(t, ms, lidJID(testLID).String()); !ok {
			t.Fatal("nil LID store: chat must stay")
		}
	})
	t.Run("merge failure is returned", func(t *testing.T) {
		r, ms := newTestResolver(t, nil, newFakeLIDs(testLID, testPN), nil)
		seedMergeable(t, ms)
		if _, err := ms.db.Exec("CREATE TRIGGER x BEFORE DELETE ON chats BEGIN SELECT RAISE(ABORT, 'step-boom'); END"); err != nil {
			t.Fatal(err)
		}
		if err := r.repair(ctx); err == nil || !strings.Contains(err.Error(), "step-boom") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("second listing fails", func(t *testing.T) {
		ms := newTestStore(t)
		lids := &hookLIDs{fakeLIDs: newFakeLIDs()}
		r := newChatResolver(ms, &fakeContacts{}, lids, nil)
		if err := ms.StoreChat(lidJID(testLID).String(), "Bob", time.Now()); err != nil {
			t.Fatal(err)
		}
		// The unmapped lookup in the first pass closes the database before the second listing
		lids.onLookup = func() { ms.Close() }
		if err := r.repair(ctx); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("renaming fails", func(t *testing.T) {
		contacts := map[string]types.ContactInfo{testPN: {FullName: "Alice"}}
		r, ms := newTestResolver(t, contacts, newFakeLIDs(), nil)
		if err := ms.StoreChat(pnJID(testPN).String(), testPN, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := ms.db.Exec("CREATE TRIGGER x BEFORE UPDATE ON chats BEGIN SELECT RAISE(ABORT, 'step-boom'); END"); err != nil {
			t.Fatal(err)
		}
		if err := r.repair(ctx); err == nil || !strings.Contains(err.Error(), "step-boom") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTriggerCoalescesAndDebounces(t *testing.T) {
	r, _ := newTestResolver(t, nil, newFakeLIDs(), nil)
	now := time.Now()
	r.now = func() time.Time { return now }
	r.debounce = time.Hour
	r.lastRun = now // a pass just ran, so the next one waits out the debounce
	r.trigger()
	r.mu.Lock()
	first := r.timer
	r.mu.Unlock()
	if first == nil {
		t.Fatal("no repair scheduled")
	}
	r.trigger() // coalesces into the pending pass
	r.mu.Lock()
	second := r.timer
	r.mu.Unlock()
	if second != first {
		t.Fatal("a second trigger must not schedule another pass")
	}
	first.Stop()
}

func TestTriggerLogsRepairFailure(t *testing.T) {
	r, ms := newTestResolver(t, nil, newFakeLIDs(), nil)
	ms.Close()
	logs, restore := redirectStderr(t)
	r.trigger()
	waitForText(t, logs, "LID chat repair failed:")
	restore()
}
