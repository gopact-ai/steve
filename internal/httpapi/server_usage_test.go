package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/readmodel"
)

type usageHTTPSource struct {
	readmodel.LedgerSource
	closed []attempt.Record
	err    error
	calls  atomic.Int32
}

func (s *usageHTTPSource) ClosedAttempts(context.Context) ([]attempt.Record, error) {
	s.calls.Add(1)
	return s.closed, s.err
}

func TestUsageEndpointIsGuardedIndependentAndFresh(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	source := &usageHTTPSource{closed: []attempt.Record{{StartedAt: start, EndedAt: start.Add(time.Second), Usage: &attempt.Usage{Reported: true, Input: 17}}}}
	// Only ClosedAttempts exists: calling Snapshot from /usage would panic.
	model := readmodel.New(readmodel.Sources{Ledger: source})
	server := serve(t, model, ServerConfig{Token: "usage-test"})
	code, _, _ := taskMetaRequest(t, server, http.MethodGet, "/usage", "", "")
	if code != http.StatusUnauthorized || source.calls.Load() != 0 {
		t.Fatalf("unguarded usage read: code=%d calls=%d", code, source.calls.Load())
	}
	for i := 1; i <= 2; i++ {
		code, headers, raw := taskMetaRequest(t, server, http.MethodGet, "/usage", "usage-test", "")
		if code != http.StatusOK || headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Type") != "application/json" {
			t.Fatalf("usage response: code=%d headers=%v body=%s", code, headers, raw)
		}
		var got readmodel.UsageSnapshot
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if source.calls.Load() != int32(i) || got.Usage == nil || got.Usage.Total.Tokens.Total != 17 || got.Sources[0].Error != "" {
			t.Fatalf("usage is cached, missing or unreadable: calls=%d response=%+v", source.calls.Load(), got)
		}
	}
}

func TestUsageEndpointRetainsReadFailureWithoutFabricatingZero(t *testing.T) {
	source := &usageHTTPSource{err: errors.New("closed attempts unavailable")}
	server := serve(t, readmodel.New(readmodel.Sources{Ledger: source}), ServerConfig{})
	code, _, raw := taskMetaRequest(t, server, http.MethodGet, "/usage", "", "")
	var got readmodel.UsageSnapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("usage response: %d %s: %v", code, raw, err)
	}
	if code != http.StatusOK || got.Usage != nil || len(got.Sources) != 1 || got.Sources[0].Error != source.err.Error() {
		t.Fatalf("usage failure became zero or lost source health: %d %s", code, raw)
	}
}

func TestStateEndpointDoesNotExposeUsage(t *testing.T) {
	server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{})
	code, _, raw := taskMetaRequest(t, server, http.MethodGet, "/state", "", "")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || code != http.StatusOK {
		t.Fatalf("state response: %d %s: %v", code, raw, err)
	}
	if _, exists := fields["usage"]; exists {
		t.Fatal("/state still exposes usage")
	}
}
