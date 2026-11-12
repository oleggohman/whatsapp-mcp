package main

import (
	"context"
	"testing"
)

func TestHistorySenderUnparseableParticipantKeepsUserPart(t *testing.T) {
	chat := pnJID(testPN)
	got := historySender(context.Background(), newFakeLIDs(), "1.2.3@s.whatsapp.net", false, chat, "self")
	if got != "1.2.3" {
		t.Fatalf("sender = %q", got)
	}
}
