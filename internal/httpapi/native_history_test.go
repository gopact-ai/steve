package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/readmodel"
)

type nativeHistoryFixture struct {
	consoleapi.Admin
	calls atomic.Int32
}

func (f *nativeHistoryFixture) NativeHistory(context.Context, string, nativehistory.Source) ([]nativehistory.Entry, error) {
	f.calls.Add(1)
	return []nativehistory.Entry{}, nil
}
func (f *nativeHistoryFixture) ImportNativeHistory(context.Context, string, consoleapi.NativeImportRequest) (consoleapi.ImportedSession, error) {
	f.calls.Add(1)
	return consoleapi.ImportedSession{Conversation: "console:import:fixture"}, nil
}

func TestNativeHistoryEndpointsRequireOwnerAndRejectMalformedImport(t *testing.T) {
	s := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: "owner-test-token"})
	f := &nativeHistoryFixture{}
	s.SetAdmin(f)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, token := range []string{"", "wrong"} {
			req, _ := http.NewRequest(method, s.URL()+"/console/nodes/worker/native-history", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unowned history request: %d", resp.StatusCode)
			}
		}
	}
	for _, body := range []string{`{"command_id":"one","unexpected":true}`, `{} {}`} {
		req, _ := http.NewRequest(http.MethodPost, s.URL()+"/console/nodes/worker/native-history", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer owner-test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("malformed import: %d", resp.StatusCode)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("rejected request reached history storage")
	}
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/console/nodes/worker/native-history?harness=codex", nil)
	req.Header.Set("Authorization", "Bearer owner-test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" || f.calls.Load() != 1 {
		t.Fatal("owner history lookup unavailable or cacheable")
	}
}
