package turn

import (
	"context"
	"github.com/gopact-ai/steve/internal/turnapi"
)

type Source = turnapi.Source
type Actor = turnapi.Actor
type ReplyContext = turnapi.ReplyContext
type Admission = turnapi.Admission
type Request = turnapi.Request

type InputExecutor interface {
	Handle(context.Context, Request) (Result, error)
	ParseInput(string) (string, ParsedInput)
}
