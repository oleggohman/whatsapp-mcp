package main

import (
	"context"
	"strings"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// normalizeJID returns the phone-number JID for a LID JID when it can be found, else jid unchanged.
// A PN alt is trusted first; otherwise the LID->PN map is consulted. PNs, groups and unknown
// LIDs pass through (device suffix dropped for LIDs that get mapped).
func normalizeJID(ctx context.Context, lids store.LIDStore, jid, alt types.JID) types.JID {
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD()
	}
	if lids == nil {
		return jid
	}
	pn, err := lids.GetPNForLID(ctx, types.NewJID(jid.User, types.HiddenUserServer))
	if err != nil || pn.IsEmpty() || pn.Server != types.DefaultUserServer {
		return jid
	}
	return pn.ToNonAD()
}

// messageChatJID returns the chat JID under which a live message should be stored.
// Groups are unchanged. For DMs the alt address is SenderAlt when incoming and RecipientAlt
// when outgoing; SenderAlt of an outgoing message is our own PN and must never be used.
func messageChatJID(ctx context.Context, lids store.LIDStore, info *types.MessageInfo) types.JID {
	if info.IsGroup || info.Chat.Server == types.GroupServer {
		return info.Chat
	}
	alt := info.SenderAlt
	if info.IsFromMe {
		alt = info.RecipientAlt
	}
	return normalizeJID(ctx, lids, info.Chat, alt)
}

// messageSenderUser returns the bare user digits of the sender, PN-normalized when possible.
func messageSenderUser(ctx context.Context, lids store.LIDStore, info *types.MessageInfo) string {
	return normalizeJID(ctx, lids, info.Sender, info.SenderAlt).User
}

// historySender returns the bare sender digits for a history-sync message.
// participant is the (possibly full-JID) participant string from the message key.
func historySender(ctx context.Context, lids store.LIDStore, participant string, fromMe bool, chat types.JID, self string) string {
	if fromMe {
		return self
	}
	if participant == "" {
		return chat.User
	}
	jid, err := types.ParseJID(participant)
	if err != nil {
		// Not a parseable JID: keep only the user part.
		user, _, _ := strings.Cut(participant, "@")
		return user
	}
	return normalizeJID(ctx, lids, jid, types.EmptyJID).User
}
