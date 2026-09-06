package acphost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/gopact-ai/acp"
)

func TestPromptSettlementDistinguishesProtocolResponseAndStreamLoss(t *testing.T) {
	rpc := &acp.Error{Code: acp.ErrorCodeInternalError, Message: "explicit agent failure"}
	for _, err := range []error{nil, ErrTurnCanceled, rpc, fmt.Errorf("wrapped: %w", rpc)} {
		if !PromptSettled(err) {
			t.Fatalf("explicit response not settled: %v", err)
		}
	}
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, context.Canceled, ErrStopUnconfirmed} {
		classified := promptFailure(err, false)
		if PromptSettled(classified) || !errors.Is(classified, ErrStopUnconfirmed) {
			t.Fatalf("stream loss claimed a response: %v", classified)
		}
	}
	if !PromptSettled(promptFailure(io.EOF, true)) {
		t.Fatal("verified original process exit was ignored")
	}
	if err := promptFailure(rpc, false); !PromptSettled(err) || errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("explicit RPC response was quarantined: %v", err)
	}
}
