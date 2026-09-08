package app

import (
	"bytes"
	"log"
	"log/slog"
	"testing"

	"github.com/gopact-ai/steve/internal/logs"
)

// captureLog routes both slog and log.Printf output into a buffer for the
// duration of the test, restoring the process-wide defaults afterwards.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previousLogger := slog.Default()
	previousWriter, previousFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(logs.NewHandler(&output)))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return &output
}
