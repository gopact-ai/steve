package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/view"
)

func TestCardDeclinesTextOnlyQuestionWithoutCreatingPendingRequest(t *testing.T) {
	g := New(fakeProcessor{})
	ui := &turnUI{g: g, cardID: "card"}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	answer, err := g.askQuestion(ctx, ui, view.Question{Message: "How should I continue?", AllowFreeText: true})
	if err != nil || answer.Decision != "decline" || answer.Chosen() {
		t.Fatalf("unsupported text-only card = %+v, %v", answer, err)
	}
	if len(g.asks) != 0 || ui.state.Question != nil || ui.state.UpdatedAt != (time.Time{}) {
		t.Fatal("unsupported card created a pending interaction")
	}
}
