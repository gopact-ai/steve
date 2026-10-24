package work

import (
	"errors"
	"io"
	"net"
	"strings"
)

// Failure classes a card names in its title, so a stuck-looking task says why:
// the link to the agent broke, the model refused for quota or rate, or the
// agent itself failed at the work.
const (
	FailTransport = "transport"
	FailQuota     = "quota"
	FailAgent     = "agent"
)

var quotaMarks = []string{"429", "rate limit", "rate_limit", "ratelimit", "too many requests", "quota", "usage limit", "insufficient_quota", "overloaded", "503", "service unavailable", "capacity"}

var transportMarks = []string{"connection refused", "connection reset", "broken pipe", "no such host", "network is unreachable", "i/o timeout", "tls handshake", "eof"}

// classify reads an execution error into one of the failure classes. Quota
// wins over transport: an upstream 503 arrives as a transport error with the
// status in its text, and the useful answer is "the model is unavailable".
// Classify is exported for callers that report failures of their own.
func Classify(err error) string { return classify(err) }

func classify(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ToLower(err.Error())
	for _, mark := range quotaMarks {
		if strings.Contains(text, mark) {
			return FailQuota
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return FailTransport
	}
	for _, mark := range transportMarks {
		if strings.Contains(text, mark) {
			return FailTransport
		}
	}
	return FailAgent
}
