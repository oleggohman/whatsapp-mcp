package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

const (
	// groupFailTTL is how long a failed group-info lookup is remembered per group.
	groupFailTTL = time.Hour
	// repairDebounce is the minimum gap between two repair passes.
	repairDebounce = 5 * time.Second
)

// groupInfoFn returns the subject of a group; it is a func so tests need no network.
type groupInfoFn func(ctx context.Context, jid types.JID) (string, error)

// nameHints are optional name sources that come with the event being processed.
type nameHints struct {
	verifiedName string // msg.Info.VerifiedName (incoming DM only)
	pushName     string // msg.Info.PushName (incoming DM only)
	convName     string // history-sync conversation Name/DisplayName
}

var (
	lidPlaceholderRe = regexp.MustCompile(`^LID \d+$`)
	numberLikeRe     = regexp.MustCompile(`^\+?[\d\s\-()]+$`)
)

// isPlaceholderName reports whether name is empty or one of the generated fallbacks:
// bare digits, a formatted number, "LID <digits>" or "Group <id>".
func isPlaceholderName(name string, jid types.JID) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == jid.User {
		return true
	}
	if lidPlaceholderRe.MatchString(name) {
		return true
	}
	if jid.Server == types.GroupServer {
		return name == "Group "+jid.User
	}
	return numberLikeRe.MatchString(name)
}

// chatResolver resolves chat names and repairs LID chats. It depends only on the
// store interfaces and a groupInfoFn, never on *whatsmeow.Client.
type chatResolver struct {
	store *MessageStore
	// The contact and LID stores are read on every use: a freshly created device gets
	// them only when it is saved after pairing, so a snapshot taken earlier stays nil.
	contactsFn func() store.ContactStore
	lidsFn     func() store.LIDStore
	groupInfo  groupInfoFn

	now      func() time.Time
	debounce time.Duration

	mu        sync.Mutex
	groupFail map[types.JID]time.Time
	timer     *time.Timer
	lastRun   time.Time
	runMu     sync.Mutex
}

func newChatResolver(ms *MessageStore, contacts store.ContactStore, lids store.LIDStore, gi groupInfoFn) *chatResolver {
	return newLazyChatResolver(ms,
		func() store.ContactStore { return contacts },
		func() store.LIDStore { return lids }, gi)
}

// newLazyChatResolver builds a resolver that fetches the stores through the given funcs on every use.
func newLazyChatResolver(ms *MessageStore, contacts func() store.ContactStore, lids func() store.LIDStore, gi groupInfoFn) *chatResolver {
	return &chatResolver{
		store:      ms,
		contactsFn: contacts,
		lidsFn:     lids,
		groupInfo:  gi,
		now:        time.Now,
		debounce:   repairDebounce,
		groupFail:  map[types.JID]time.Time{},
	}
}

// newDeviceChatResolver builds a resolver that always uses the current stores of dev.
func newDeviceChatResolver(ms *MessageStore, dev *store.Device, gi groupInfoFn) *chatResolver {
	return newLazyChatResolver(ms,
		func() store.ContactStore { return dev.Contacts },
		func() store.LIDStore { return dev.LIDs }, gi)
}

// lidStore returns the current LID store (nil before the device is saved).
func (r *chatResolver) lidStore() store.LIDStore { return r.lidsFn() }

// contactStore returns the current contact store (nil before the device is saved).
func (r *chatResolver) contactStore() store.ContactStore { return r.contactsFn() }

// contactFor merges the contact records of the PN and LID forms of jid; for every field
// the PN record wins and the LID record fills the gaps.
func (r *chatResolver) contactFor(ctx context.Context, jid types.JID) types.ContactInfo {
	var pn, lid types.JID
	lids, contacts := r.lidStore(), r.contactStore()
	switch jid.Server {
	case types.HiddenUserServer:
		lid = jid.ToNonAD()
		if lids != nil {
			if m, err := lids.GetPNForLID(ctx, lid); err == nil && !m.IsEmpty() {
				pn = m.ToNonAD()
			}
		}
	default:
		pn = jid.ToNonAD()
		if lids != nil {
			if m, err := lids.GetLIDForPN(ctx, pn); err == nil && !m.IsEmpty() {
				lid = m.ToNonAD()
			}
		}
	}
	var out types.ContactInfo
	for _, j := range []types.JID{pn, lid} {
		if j.IsEmpty() || contacts == nil {
			continue
		}
		c, err := contacts.GetContact(ctx, j)
		if err != nil {
			continue
		}
		if out.FullName == "" {
			out.FullName = c.FullName
		}
		if out.FirstName == "" {
			out.FirstName = c.FirstName
		}
		if out.BusinessName == "" {
			out.BusinessName = c.BusinessName
		}
		if out.PushName == "" {
			out.PushName = c.PushName
		}
	}
	return out
}

// resolveName picks the best available name for jid (no database access).
func (r *chatResolver) resolveName(ctx context.Context, jid types.JID, h nameHints) string {
	if jid.Server == types.GroupServer {
		return r.resolveGroupName(ctx, jid, h)
	}
	c := r.contactFor(ctx, jid)
	for _, n := range []string{c.FullName, c.FirstName, c.BusinessName, h.verifiedName, c.PushName, h.pushName, h.convName} {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	if jid.Server == types.HiddenUserServer {
		return "LID " + jid.User
	}
	return "+" + jid.User
}

func (r *chatResolver) resolveGroupName(ctx context.Context, jid types.JID, h nameHints) string {
	if n := strings.TrimSpace(h.convName); n != "" {
		return n
	}
	if r.groupInfo != nil && !r.groupLookupCached(jid) {
		name, err := r.groupInfo(ctx, jid)
		if err == nil && strings.TrimSpace(name) != "" {
			return name
		}
		r.mu.Lock()
		r.groupFail[jid] = r.now()
		r.mu.Unlock()
	}
	return fmt.Sprintf("Group %s", jid.User)
}

// groupLookupCached reports whether a failed lookup for jid is still within groupFailTTL.
func (r *chatResolver) groupLookupCached(jid types.JID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.groupFail[jid]
	return ok && r.now().Sub(at) < groupFailTTL
}

// chatName returns the name to store for a chat: a stored real name is kept, a stored
// placeholder (or no row) is re-resolved.
func (r *chatResolver) chatName(ctx context.Context, jid types.JID, h nameHints) string {
	existing := r.store.getChatName(jid.String())
	if !isPlaceholderName(existing, jid) {
		return existing
	}
	name := r.resolveName(ctx, jid, h)
	if isPlaceholderName(name, jid) && existing != "" {
		return existing
	}
	return name
}

// mergeIfMapped merges a stored LID chat into its PN chat once orig (LID) was mapped to norm (PN).
func (r *chatResolver) mergeIfMapped(ctx context.Context, orig, norm types.JID) {
	if orig.Server != types.HiddenUserServer || norm.Server != types.DefaultUserServer {
		return
	}
	lid := types.NewJID(orig.User, types.HiddenUserServer).String()
	if !r.chatExists(lid) {
		return
	}
	if err := r.store.mergeChat(ctx, lid, norm.ToNonAD().String(), r.lidStore()); err != nil {
		logf("Failed to merge chat %s into %s: %v\n", lid, norm, err)
	}
}

// lockChat takes the repair lock for a chat a repair may merge (anything but a group) and
// returns the unlock func. Holding it from StoreChat to StoreMessage keeps a repair from
// deleting the LID chat in between, which would fail the message insert (foreign key).
// Groups are never merged, and resolving their names may need the network.
func (r *chatResolver) lockChat(jid types.JID) func() {
	if jid.Server == types.GroupServer {
		return func() {}
	}
	r.runMu.Lock()
	return r.runMu.Unlock
}

func (r *chatResolver) chatExists(jid string) bool {
	var n int
	return r.store.db.QueryRow("SELECT 1 FROM chats WHERE jid = ?", jid).Scan(&n) == nil
}

// repair merges every LID chat that now has a PN mapping and re-resolves placeholder
// names of individual chats. It is idempotent and safe to call repeatedly.
func (r *chatResolver) repair(ctx context.Context) error {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	lidChats, err := r.store.chatsWithName("@" + types.HiddenUserServer)
	if err != nil {
		return err
	}
	lids := r.lidStore()
	for _, c := range lidChats {
		jid, err := types.ParseJID(c[0])
		if err != nil || lids == nil {
			continue
		}
		pn, err := lids.GetPNForLID(ctx, jid)
		if err != nil || pn.IsEmpty() || pn.Server != types.DefaultUserServer {
			continue
		}
		if err := r.store.mergeChat(ctx, c[0], pn.ToNonAD().String(), lids); err != nil {
			return err
		}
	}

	// Groups are skipped here: resolving them may need a network call per chat.
	chats, err := r.store.chatsWithName("")
	if err != nil {
		return err
	}
	for _, c := range chats {
		jid, err := types.ParseJID(c[0])
		if err != nil || jid.Server == types.GroupServer || !isPlaceholderName(c[1], jid) {
			continue
		}
		name := r.resolveName(ctx, jid, nameHints{})
		if name != c[1] && !isPlaceholderName(name, jid) {
			if err := r.store.updateChatName(c[0], name); err != nil {
				return err
			}
		}
	}
	return nil
}

// trigger schedules a debounced repair pass; calls while one is pending coalesce into it.
func (r *chatResolver) trigger() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		return
	}
	delay := time.Duration(0)
	if since := r.now().Sub(r.lastRun); since < r.debounce {
		delay = r.debounce - since
	}
	r.timer = time.AfterFunc(delay, func() {
		r.mu.Lock()
		r.timer = nil
		r.lastRun = r.now()
		r.mu.Unlock()
		if err := r.repair(context.Background()); err != nil {
			logf("LID chat repair failed: %v\n", err)
		}
	})
}
