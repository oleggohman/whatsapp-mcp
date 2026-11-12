package main

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// Synthetic numbers only; none of these belong to a real account.
const (
	testPN    = "15550100001"
	testLID   = "99900000001"
	testPN2   = "15550100002"
	testLID2  = "99900000002"
	testMyPN  = "15550199999"
	testMyLID = "99900099999"
)

func pnJID(user string) types.JID  { return types.NewJID(user, types.DefaultUserServer) }
func lidJID(user string) types.JID { return types.NewJID(user, types.HiddenUserServer) }

// fakeLIDs is an in-memory store.LIDStore; unimplemented methods panic via the nil embed.
type fakeLIDs struct {
	store.LIDStore
	lidToPN map[string]string
	pnToLID map[string]string
}

func newFakeLIDs(pairs ...string) *fakeLIDs {
	f := &fakeLIDs{lidToPN: map[string]string{}, pnToLID: map[string]string{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		f.add(pairs[i], pairs[i+1])
	}
	return f
}

func (f *fakeLIDs) add(lid, pn string) {
	f.lidToPN[lid] = pn
	f.pnToLID[pn] = lid
}

func (f *fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	if pn, ok := f.lidToPN[lid.User]; ok {
		return pnJID(pn), nil
	}
	return types.EmptyJID, nil
}

func (f *fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	if lid, ok := f.pnToLID[pn.User]; ok {
		return lidJID(lid), nil
	}
	return types.EmptyJID, nil
}

// fakeContacts is an in-memory store.ContactStore keyed by user digits.
type fakeContacts struct {
	store.ContactStore
	m map[string]types.ContactInfo
}

func (f *fakeContacts) GetContact(_ context.Context, user types.JID) (types.ContactInfo, error) {
	if c, ok := f.m[user.User]; ok {
		c.Found = true
		return c, nil
	}
	return types.ContactInfo{}, nil
}

func TestNormalizeJID(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs(testLID, testPN)

	cases := []struct {
		name string
		jid  types.JID
		alt  types.JID
		want types.JID
	}{
		{"PN passthrough", pnJID(testPN), types.EmptyJID, pnJID(testPN)},
		{"PN ignores alt", pnJID(testPN), pnJID(testPN2), pnJID(testPN)},
		{"LID with PN alt", lidJID(testLID2), pnJID(testPN2), pnJID(testPN2)},
		{"LID via map", lidJID(testLID), types.EmptyJID, pnJID(testPN)},
		{"LID alt that is itself a LID falls to map", lidJID(testLID), lidJID(testLID2), pnJID(testPN)},
		{"unknown LID unchanged", lidJID(testLID2), types.EmptyJID, lidJID(testLID2)},
		{"group untouched", types.NewJID("12345-678", types.GroupServer), pnJID(testPN), types.NewJID("12345-678", types.GroupServer)},
		{"device suffix dropped", types.JID{User: testLID, Server: types.HiddenUserServer, Device: 3}, types.EmptyJID, pnJID(testPN)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeJID(ctx, lids, c.jid, c.alt); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestNormalizeJIDNilStore(t *testing.T) {
	got := normalizeJID(context.Background(), nil, lidJID(testLID), types.EmptyJID)
	if got != lidJID(testLID) {
		t.Fatalf("got %v", got)
	}
}

func TestMessageChatJIDIncomingUsesSenderAlt(t *testing.T) {
	info := &types.MessageInfo{MessageSource: types.MessageSource{
		Chat: lidJID(testLID2), Sender: lidJID(testLID2), SenderAlt: pnJID(testPN2),
	}}
	if got := messageChatJID(context.Background(), newFakeLIDs(), info); got != pnJID(testPN2) {
		t.Fatalf("got %v", got)
	}
}

func TestMessageChatJIDOutgoingUsesRecipientAlt(t *testing.T) {
	info := &types.MessageInfo{MessageSource: types.MessageSource{
		Chat: lidJID(testLID2), Sender: lidJID(testMyLID), IsFromMe: true,
		SenderAlt: pnJID(testMyPN), RecipientAlt: pnJID(testPN2),
	}}
	if got := messageChatJID(context.Background(), newFakeLIDs(), info); got != pnJID(testPN2) {
		t.Fatalf("got %v", got)
	}
}

// An outgoing DM with no RecipientAlt must never be mapped to our own PN (SenderAlt).
func TestMessageChatJIDOutgoingNeverUsesSenderAlt(t *testing.T) {
	info := &types.MessageInfo{MessageSource: types.MessageSource{
		Chat: lidJID(testLID2), Sender: lidJID(testMyLID), IsFromMe: true, SenderAlt: pnJID(testMyPN),
	}}
	got := messageChatJID(context.Background(), newFakeLIDs(), info)
	if got != lidJID(testLID2) {
		t.Fatalf("got %v, want chat to stay the LID", got)
	}
	// With a map entry the fallback works.
	got = messageChatJID(context.Background(), newFakeLIDs(testLID2, testPN2), info)
	if got != pnJID(testPN2) {
		t.Fatalf("got %v", got)
	}
}

func TestMessageChatJIDGroupUnchanged(t *testing.T) {
	g := types.NewJID("123-456", types.GroupServer)
	info := &types.MessageInfo{MessageSource: types.MessageSource{Chat: g, Sender: lidJID(testLID), IsGroup: true, SenderAlt: pnJID(testPN)}}
	if got := messageChatJID(context.Background(), newFakeLIDs(), info); got != g {
		t.Fatalf("got %v", got)
	}
}

func TestMessageSenderUserGroupLIDUsesSenderAlt(t *testing.T) {
	info := &types.MessageInfo{MessageSource: types.MessageSource{
		Chat: types.NewJID("123-456", types.GroupServer), IsGroup: true,
		Sender: lidJID(testLID), SenderAlt: pnJID(testPN),
	}}
	if got := messageSenderUser(context.Background(), newFakeLIDs(), info); got != testPN {
		t.Fatalf("got %q want %q", got, testPN)
	}
}

func TestHistorySenderStoresBareDigits(t *testing.T) {
	ctx := context.Background()
	lids := newFakeLIDs(testLID, testPN)
	group := types.NewJID("123-456", types.GroupServer)

	// Full LID participant JID -> PN digits.
	if got := historySender(ctx, lids, lidJID(testLID).String(), false, group, testMyPN); got != testPN {
		t.Fatalf("LID participant: got %q", got)
	}
	// Full PN participant JID -> bare digits.
	if got := historySender(ctx, lids, pnJID(testPN2).String(), false, group, testMyPN); got != testPN2 {
		t.Fatalf("PN participant: got %q", got)
	}
	// Unknown LID participant stays LID digits (never a full JID).
	if got := historySender(ctx, lids, lidJID(testLID2).String(), false, group, testMyPN); got != testLID2 {
		t.Fatalf("unknown LID participant: got %q", got)
	}
	// Outgoing -> own user.
	if got := historySender(ctx, lids, "", true, pnJID(testPN), testMyPN); got != testMyPN {
		t.Fatalf("from me: got %q", got)
	}
	// DM without participant -> chat user.
	if got := historySender(ctx, lids, "", false, pnJID(testPN), testMyPN); got != testPN {
		t.Fatalf("dm: got %q", got)
	}
}
