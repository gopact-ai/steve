package consoleapi

import "github.com/gopact-ai/steve/internal/conversationapi"

type ExchangeState = conversationapi.ExchangeState

const (
	ExchangeQueued       = conversationapi.ExchangeQueued
	ExchangeRunning      = conversationapi.ExchangeRunning
	ExchangeRecovering   = conversationapi.ExchangeRecovering
	ExchangeAwaitingUser = conversationapi.ExchangeAwaitingUser
	ExchangeDone         = conversationapi.ExchangeDone
	ExchangeFailed       = conversationapi.ExchangeFailed
	ExchangeCancelled    = conversationapi.ExchangeCancelled
)
