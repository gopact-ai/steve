package consoleapi

import "context"

// AttemptQueries is the native execution history used by task details and
// conversation files. It is separate from task accounting rows.
type AttemptQueries interface {
	QueryAttempts(context.Context, AttemptHistoryQuery) (AttemptHistoryPage, error)
}

type AttemptHistoryQuery struct {
	TaskID       string `json:"task_id,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type AttemptHistoryItem struct {
	AttemptView
	TaskID  string `json:"task_id"`
	Project string `json:"project,omitempty"`
	// A missing file count does not establish that no files changed.
	FilesKnown     bool   `json:"files_known"`
	FilesTruncated bool   `json:"files_truncated,omitempty"`
	FilesError     string `json:"files_error,omitempty"`
}

type AttemptHistoryPage struct {
	Items      []AttemptHistoryItem `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}
