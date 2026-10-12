package consoleapi

import "github.com/gopact-ai/steve/internal/conversationapi"

type QuestionOption = conversationapi.QuestionOption
type QuestionAnswer = conversationapi.QuestionAnswer
type PendingQuestion = conversationapi.PendingQuestion
type Interactions = conversationapi.Interactions
type Submission = conversationapi.Submission
type Submissions = conversationapi.Submissions

var (
	ErrQuestionNotFound  = conversationapi.ErrQuestionNotFound
	ErrQuestionConflict  = conversationapi.ErrQuestionConflict
	ErrInvalidAnswer     = conversationapi.ErrInvalidAnswer
	ErrQuestionForbidden = conversationapi.ErrQuestionForbidden
	ErrConsoleClosing    = conversationapi.ErrConsoleClosing
)
