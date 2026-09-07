package consoleapi

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/material"
)

var (
	ErrQuestionNotFound  = errors.New("question not found")
	ErrQuestionConflict  = errors.New("question has already been resolved")
	ErrInvalidAnswer     = errors.New("invalid question answer")
	ErrQuestionForbidden = errors.New("question belongs to another principal")
	ErrConsoleClosing    = errors.New("console is shutting down")
)

type QuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

// QuestionAnswer is a user decision, never a new prompt for the running agent.
type QuestionAnswer struct {
	CommandID string `json:"command_id"`
	Choice    string `json:"choice,omitempty"`
	Text      string `json:"text,omitempty"`
	Decision  string `json:"decision"` // accept | decline | cancel
}

type PendingQuestion struct {
	ID            string           `json:"id"`
	Conversation  string           `json:"conversation"`
	ExchangeID    string           `json:"exchange_id"`
	Project       string           `json:"project,omitempty"`
	TaskID        string           `json:"task_id,omitempty"`
	AttemptID     string           `json:"attempt_id,omitempty"`
	SessionID     string           `json:"session_id,omitempty"`
	Generation    uint64           `json:"generation,omitempty"`
	ToolCallID    string           `json:"tool_call_id,omitempty"`
	RequestID     string           `json:"request_id,omitempty"`
	Principal     string           `json:"principal"`
	Kind          string           `json:"kind"` // permission | question
	Title         string           `json:"title,omitempty"`
	Message       string           `json:"message"`
	Options       []QuestionOption `json:"options"`
	AllowFreeText bool             `json:"allow_free_text"`
	Required      bool             `json:"required"`
	Locale        string           `json:"locale,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	Deadline      time.Time        `json:"deadline"`
	UpdatedAt     time.Time        `json:"updated_at"`
	State         string           `json:"state"` // pending | answered | declined | cancelled | expired | interrupted
	Answer        *QuestionAnswer  `json:"answer,omitempty"`
}

type Interactions interface {
	Questions(conversation string) []PendingQuestion
	AnswerQuestion(context.Context, string, QuestionAnswer) (PendingQuestion, error)
}

type Submission struct {
	Conversation string         `json:"conversation"`
	Input        string         `json:"input"`
	CommandID    string         `json:"command_id"`
	Quotes       []QuoteRef     `json:"quotes,omitempty"`
	Refs         []material.Ref `json:"refs,omitempty"`
	Locale       string         `json:"locale,omitempty"`
}

type Submissions interface {
	Submit(context.Context, Submission) (Exchange, error)
	SendSubmission(context.Context, Submission) (Reply, error)
}
