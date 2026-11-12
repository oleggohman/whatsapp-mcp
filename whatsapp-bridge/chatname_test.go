package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func newTestResolver(t *testing.T, contacts map[string]types.ContactInfo, lids *fakeLIDs, gi groupInfoFn) (*chatResolver, *MessageStore) {
	t.Helper()
	ms := newTestStore(t)
	r := newChatResolver(ms, &fakeContacts{m: contacts}, lids, gi)
	r.debounce = 0
	return r, ms
}

// Reproduction of the user-reported bug: the chat is a LID, the address-book name sits
// under the PN contact, and the LID->PN mapping is in the LID map.
func TestReproLIDChatNamedFromPNContact(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs(testLID, testPN)
	r, _ := newTestResolver(t, map[string]types.ContactInfo{testPN: {FullName: "HY.group Support"}}, lids, nil)

	info := &types.MessageInfo{MessageSource: types.MessageSource{Chat: lidJID(testLID), Sender: lidJID(testLID)}}
	chat := messageChatJID(ctx, lids, info)
	if chat != pnJID(testPN) {
		t.Fatalf("chat not normalized: %v", chat)
	}
	if got := r.chatName(ctx, chat, nameHints{}); got != "HY.group Support" {
		t.Fatalf("name = %q", got)
	}
	// Even if the caller kept the LID, resolution consults the PN contact.
	if got := r.resolveName(ctx, lidJID(testLID), nameHints{}); got != "HY.group Support" {
		t.Fatalf("name for LID jid = %q", got)
	}
}

func TestResolveNameOrder(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs()
	full := types.ContactInfo{FullName: "Full", FirstName: "First", BusinessName: "Biz", PushName: "Push"}
	cases := []struct {
		name     string
		contact  types.ContactInfo
		hints    nameHints
		want     string
		jid      types.JID
		wantFall bool
	}{
		{"full name first", full, nameHints{verifiedName: "V", pushName: "P", convName: "C"}, "Full", pnJID(testPN), false},
		{"first name", types.ContactInfo{FirstName: "First", BusinessName: "Biz"}, nameHints{}, "First", pnJID(testPN), false},
		{"business name", types.ContactInfo{BusinessName: "Biz", PushName: "Push"}, nameHints{verifiedName: "V"}, "Biz", lidJID(testLID), false},
		{"verified name", types.ContactInfo{PushName: "Push"}, nameHints{verifiedName: "V", pushName: "P"}, "V", lidJID(testLID), false},
		{"contact push name", types.ContactInfo{PushName: "Push"}, nameHints{pushName: "P", convName: "C"}, "Push", lidJID(testLID), false},
		{"message push name", types.ContactInfo{}, nameHints{pushName: "P", convName: "C"}, "P", lidJID(testLID), false},
		{"conversation name", types.ContactInfo{}, nameHints{convName: "C"}, "C", pnJID(testPN), false},
		{"PN fallback", types.ContactInfo{}, nameHints{}, "+" + testPN, pnJID(testPN), true},
		{"LID fallback", types.ContactInfo{}, nameHints{}, "LID " + testLID, lidJID(testLID), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newTestResolver(t, map[string]types.ContactInfo{c.jid.User: c.contact}, lids, nil)
			got := r.resolveName(ctx, c.jid, c.hints)
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
			if isPlaceholderName(got, c.jid) != c.wantFall {
				t.Fatalf("isPlaceholderName(%q) mismatch", got)
			}
		})
	}
}

// A LID-only contact with just a push/business name resolves to that name.
func TestResolveNameLIDContactOnly(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestResolver(t, map[string]types.ContactInfo{testLID: {BusinessName: "Shop"}}, newFakeLIDs(), nil)
	if got := r.resolveName(ctx, lidJID(testLID), nameHints{}); got != "Shop" {
		t.Fatalf("got %q", got)
	}
	r2, _ := newTestResolver(t, nil, newFakeLIDs(), nil)
	if got := r2.resolveName(ctx, lidJID(testLID), nameHints{pushName: "Pusher"}); got != "Pusher" {
		t.Fatalf("got %q", got)
	}
}

func TestIsPlaceholderName(t *testing.T) {
	g := types.NewJID("123-456", types.GroupServer)
	cases := []struct {
		name string
		jid  types.JID
		want bool
	}{
		{"", pnJID(testPN), true},
		{testPN, pnJID(testPN), true},
		{"+" + testPN, pnJID(testPN), true},
		{"+1 555 010-0001", pnJID(testPN), true},
		{"LID " + testLID, lidJID(testLID), true},
		{"Group 123-456", g, true},
		{"Group chat", g, false},
		{"Alice", pnJID(testPN), false},
		{"HY.group Support", lidJID(testLID), false},
		{"Group of friends", pnJID(testPN), false},
	}
	for _, c := range cases {
		if got := isPlaceholderName(c.name, c.jid); got != c.want {
			t.Errorf("isPlaceholderName(%q, %v) = %v, want %v", c.name, c.jid, got, c.want)
		}
	}
}

func TestChatNamePlaceholderUpgradedRealKept(t *testing.T) {
	ctx := context.Background()
	contacts := map[string]types.ContactInfo{}
	r, ms := newTestResolver(t, contacts, newFakeLIDs(), nil)
	now := time.Now().UTC()
	jid := pnJID(testPN)

	// Stored placeholder is replaced once a real name appears.
	if err := ms.StoreChat(jid.String(), "+"+testPN, now); err != nil {
		t.Fatal(err)
	}
	contacts[testPN] = types.ContactInfo{FullName: "Alice"}
	if got := r.chatName(ctx, jid, nameHints{}); got != "Alice" {
		t.Fatalf("placeholder not upgraded: %q", got)
	}

	// A stored real name is never replaced, not even by another real name.
	if err := ms.StoreChat(jid.String(), "Stored Real", now); err != nil {
		t.Fatal(err)
	}
	if got := r.chatName(ctx, jid, nameHints{}); got != "Stored Real" {
		t.Fatalf("real name overwritten: %q", got)
	}

	// A stored real name is never replaced by a placeholder.
	delete(contacts, testPN)
	if got := r.chatName(ctx, jid, nameHints{}); got != "Stored Real" {
		t.Fatalf("real name lost: %q", got)
	}
}

func TestGroupNameResolutionAndFailureCache(t *testing.T) {
	ctx := context.Background()
	g := types.NewJID("123-456", types.GroupServer)
	calls := 0
	var gErr error = errors.New("not in group")
	gi := func(_ context.Context, j types.JID) (string, error) {
		calls++
		if gErr != nil {
			return "", gErr
		}
		return "Team Chat", nil
	}
	r, _ := newTestResolver(t, nil, newFakeLIDs(), gi)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return clock }

	// conversation name wins and needs no network.
	if got := r.resolveName(ctx, g, nameHints{convName: "From History"}); got != "From History" || calls != 0 {
		t.Fatalf("got %q calls=%d", got, calls)
	}
	// failure -> fallback, then cached for an hour.
	if got := r.resolveName(ctx, g, nameHints{}); got != "Group 123-456" || calls != 1 {
		t.Fatalf("got %q calls=%d", got, calls)
	}
	r.resolveName(ctx, g, nameHints{})
	r.resolveName(ctx, g, nameHints{})
	if calls != 1 {
		t.Fatalf("failure not cached, calls=%d", calls)
	}
	// after the TTL the lookup is retried and may succeed.
	clock = clock.Add(61 * time.Minute)
	gErr = nil
	if got := r.resolveName(ctx, g, nameHints{}); got != "Team Chat" || calls != 2 {
		t.Fatalf("got %q calls=%d", got, calls)
	}
}

func putMsg(t *testing.T, ms *MessageStore, id, chat, sender, content string, ts time.Time) {
	t.Helper()
	if err := ms.StoreMessage(id, chat, sender, content, ts, false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
}

func chatRow(t *testing.T, ms *MessageStore, jid string) (name string, last time.Time, ok bool) {
	t.Helper()
	err := ms.db.QueryRow("SELECT name, last_message_time FROM chats WHERE jid = ?", jid).Scan(&name, &last)
	if err != nil {
		return "", time.Time{}, false
	}
	return name, last, true
}

func countMsgs(t *testing.T, ms *MessageStore, chat string) int {
	t.Helper()
	var n int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid = ?", chat).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMergeChatDuplicatesAndNames(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs(testLID, testPN)
	ms := newTestStore(t)
	lid, pn := lidJID(testLID).String(), pnJID(testPN).String()
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	// LID chat: placeholder name, newer last message; m1 also exists under the PN chat.
	if err := ms.StoreChat(lid, testLID, t2); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreChat(pn, "Alice", t1); err != nil {
		t.Fatal(err)
	}
	putMsg(t, ms, "m1", lid, testLID, "dup-from-lid", t1)
	putMsg(t, ms, "m2", lid, testLID, "only-lid", t2)
	putMsg(t, ms, "m1", pn, testPN, "dup-from-pn", t1)
	putMsg(t, ms, "m3", pn, testPN, "only-pn", t1)

	if err := ms.mergeChat(ctx, lid, pn, lids); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := chatRow(t, ms, lid); ok {
		t.Fatal("LID chat still present")
	}
	if got := countMsgs(t, ms, lid); got != 0 {
		t.Fatalf("messages left under LID: %d", got)
	}
	if got := countMsgs(t, ms, pn); got != 3 {
		t.Fatalf("messages under PN = %d, want 3", got)
	}
	var content string
	if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='m1' AND chat_jid=?", pn).Scan(&content); err != nil || content != "dup-from-pn" {
		t.Fatalf("PN copy of duplicate must win: %q %v", content, err)
	}
	var sender string
	if err := ms.db.QueryRow("SELECT sender FROM messages WHERE id='m2' AND chat_jid=?", pn).Scan(&sender); err != nil || sender != testPN {
		t.Fatalf("LID sender not mapped to PN digits: %q %v", sender, err)
	}
	name, last, _ := chatRow(t, ms, pn)
	if name != "Alice" || !last.Equal(t2) {
		t.Fatalf("name=%q last=%v", name, last)
	}

	// Second run is a no-op.
	if err := ms.mergeChat(ctx, lid, pn, lids); err != nil {
		t.Fatal(err)
	}
	if got := countMsgs(t, ms, pn); got != 3 {
		t.Fatalf("second merge changed messages: %d", got)
	}
}

// Rename-only: no PN chat exists yet, so the PN row must be created before messages move (FK on).
func TestMergeChatRenameOnly(t *testing.T) {
	ctx := context.Background()
	ms := newTestStore(t)
	lid, pn := lidJID(testLID).String(), pnJID(testPN).String()
	ts := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := ms.StoreChat(lid, "Real Name", ts); err != nil {
		t.Fatal(err)
	}
	putMsg(t, ms, "a", lid, testLID, "hi", ts)

	if err := ms.mergeChat(ctx, lid, pn, newFakeLIDs(testLID, testPN)); err != nil {
		t.Fatal(err)
	}
	name, last, ok := chatRow(t, ms, pn)
	if !ok || name != "Real Name" || !last.Equal(ts) {
		t.Fatalf("pn chat: ok=%v name=%q last=%v", ok, name, last)
	}
	if countMsgs(t, ms, pn) != 1 || countMsgs(t, ms, lid) != 0 {
		t.Fatal("messages not moved")
	}
	if _, _, ok := chatRow(t, ms, lid); ok {
		t.Fatal("LID chat remains")
	}
}

func TestMergeChatPrefersRealNameOverPlaceholder(t *testing.T) {
	ctx := context.Background()
	ms := newTestStore(t)
	lid, pn := lidJID(testLID).String(), pnJID(testPN).String()
	ts := time.Now().UTC()
	// PN row is a placeholder, LID row has a real name -> real name wins.
	ms.StoreChat(lid, "Support Desk", ts)
	ms.StoreChat(pn, testPN, ts)
	if err := ms.mergeChat(ctx, lid, pn, newFakeLIDs()); err != nil {
		t.Fatal(err)
	}
	if name, _, _ := chatRow(t, ms, pn); name != "Support Desk" {
		t.Fatalf("name = %q", name)
	}
}

func TestRepairEmptyMapThenMapped(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs()
	r, ms := newTestResolver(t, nil, lids, nil)
	lid, pn := lidJID(testLID).String(), pnJID(testPN).String()
	ts := time.Now().UTC()
	ms.StoreChat(lid, "LID "+testLID, ts)
	putMsg(t, ms, "x", lid, testLID, "hello", ts)

	if err := r.repair(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := chatRow(t, ms, lid); !ok {
		t.Fatal("repair with empty map must leave the LID chat alone")
	}
	if _, _, ok := chatRow(t, ms, pn); ok {
		t.Fatal("PN chat created without a mapping")
	}

	lids.add(testLID, testPN)
	r.trigger()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, _, ok := chatRow(t, ms, lid); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("trigger did not merge the LID chat")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if countMsgs(t, ms, pn) != 1 {
		t.Fatal("message not under PN chat")
	}
}

func TestRepairReResolvesPlaceholderNames(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs(testLID, testPN)
	r, ms := newTestResolver(t, map[string]types.ContactInfo{testPN: {FullName: "HY.group Support"}}, lids, nil)
	lid := lidJID(testLID).String()
	ms.StoreChat(lid, testLID, time.Now().UTC()) // the live-data shape: LID digits as the name
	if err := r.repair(ctx); err != nil {
		t.Fatal(err)
	}
	name, _, ok := chatRow(t, ms, pnJID(testPN).String())
	if !ok || name != "HY.group Support" {
		t.Fatalf("ok=%v name=%q", ok, name)
	}
}

func TestMergeIfMappedIncremental(t *testing.T) {
	ctx := context.Background()
	r, ms := newTestResolver(t, nil, newFakeLIDs(testLID, testPN), nil)
	lid := lidJID(testLID).String()
	ms.StoreChat(lid, "Old", time.Now().UTC())
	putMsg(t, ms, "x", lid, testLID, "hello", time.Now().UTC())
	r.mergeIfMapped(ctx, lidJID(testLID), pnJID(testPN))
	if _, _, ok := chatRow(t, ms, lid); ok {
		t.Fatal("LID chat not merged")
	}
	if countMsgs(t, ms, pnJID(testPN).String()) != 1 {
		t.Fatal("message not moved")
	}
}

// A second connection holding an open read cursor must not block the repair (WAL + busy timeout).
func TestRepairWhileReaderOpen(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs(testLID, testPN)
	r, ms := newTestResolver(t, nil, lids, nil)
	lid := lidJID(testLID).String()
	ms.StoreChat(lid, "Name", time.Now().UTC())
	putMsg(t, ms, "x", lid, testLID, "hello", time.Now().UTC())

	reader, err := openMessagesDB(messagesDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.Query("SELECT id FROM messages")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected a row")
	}

	done := make(chan error, 1)
	go func() { done <- r.repair(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("repair blocked by open reader")
	}
	if _, _, ok := chatRow(t, ms, lid); ok {
		t.Fatal("LID chat not merged")
	}
}
