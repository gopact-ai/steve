package gateway

import "testing"

func TestConversationBusyFollowsOriginalInputOwnership(t *testing.T) {
	g := New(nil)
	if g.ConversationBusy("original") {
		t.Fatal("idle conversation was busy")
	}
	claim, err := g.claimOrdinary(t.Context(), "input", "original", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !g.ConversationBusy("original") || g.ConversationBusy("console:original") {
		t.Fatal("input ownership lost its original identity")
	}
	claim.close()
	if g.ConversationBusy("original") {
		t.Fatal("released input still busy")
	}
}
