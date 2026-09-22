package task

import (
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
)

func TestBeginTurnPersistsInputIdentityBeforeAttemptAdmission(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "resume"}[resumed], func(t *testing.T) {
			book, s, before := resumeOwner(t)
			input := TurnInput{Address: channel.Address{Channel: before.Transport, Conversation: before.Channel, Message: "exact-input"}, TurnID: "exact-input"}
			if resumed {
				input.Continuation = true
				input.ResumeAdmission = ResumeAdmission{ID: "resume", TaskID: before.ID, Epoch: before.ExecutionEpoch + 1}
			} else {
				input.TurnID = ""
			}
			if _, err := s.Resume(before.ID, before.ExecutionEpoch, before.State, input.ResumeAdmission); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BeginTurn(before.ID, "worker", "node", input); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Finish(before.ID, OutcomeError, Tokens{}, 0); err != nil {
				t.Fatal(err)
			}
			s, err := OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := s.Get(before.ID)
			if len(got.Attempts) != 1 || got.Attempts[0].TurnID != input.Address.Message || got.Attempts[0].Open() || got.Attempts[0].ExecutionID != "" {
				t.Fatalf("pre-admission failure lost exact input identity: %+v", got.Attempts)
			}
		})
	}
}
