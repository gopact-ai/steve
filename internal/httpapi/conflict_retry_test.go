package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

type fakeRetryAdmin struct {
	consoleapi.Admin
	calls [][2]string
	err   error
}

func (a *fakeRetryAdmin) RetryConflict(_ context.Context, artifactID, landing string) error {
	a.calls = append(a.calls, [2]string{artifactID, landing})
	return a.err
}

// Retrying a stuck result names the landing the console saw it stuck on.
// A result that has moved on since is a stale view (409), not a bad
// request, so the console can refresh instead of showing an error.
func TestRetryConflictNamesTheLandingAndReportsAStaleView(t *testing.T) {
	admin := &fakeRetryAdmin{}
	server := taskMetaServer(t, admin)
	const path = "/console/conflicts/abc123/retry"

	if status, _, _ := taskMetaRequest(t, server, http.MethodPost, path, "owner", `{}`); status != http.StatusBadRequest {
		t.Fatalf("retry without a landing = %d; want 400", status)
	}
	if status, _, _ := taskMetaRequest(t, server, http.MethodPost, path, "owner", `not json`); status != http.StatusBadRequest {
		t.Fatalf("retry with a malformed body = %d; want 400", status)
	}
	if len(admin.calls) != 0 {
		t.Fatalf("a refused request reached the admin: %v", admin.calls)
	}

	if status, _, _ := taskMetaRequest(t, server, http.MethodPost, path, "owner", `{"landing":"land-1"}`); status != http.StatusOK {
		t.Fatalf("retry = %d; want 200", status)
	}
	if len(admin.calls) != 1 || admin.calls[0] != [2]string{"abc123", "land-1"} {
		t.Fatalf("calls = %v", admin.calls)
	}

	admin.err = fmt.Errorf("%w: moved on", artifact.ErrNotBlocked)
	if status, _, _ := taskMetaRequest(t, server, http.MethodPost, path, "owner", `{"landing":"land-1"}`); status != http.StatusConflict {
		t.Fatalf("retry of a result no longer stuck = %d; want 409", status)
	}
	admin.err = errors.New("conflict resolution is not wired")
	if status, _, body := taskMetaRequest(t, server, http.MethodPost, path, "owner", `{"landing":"land-1"}`); status != http.StatusBadRequest || len(body) == 0 {
		t.Fatalf("retry that failed = %d %q; want 400 with a reason", status, body)
	}
	if status, _, _ := taskMetaRequest(t, server, http.MethodPost, path, "", `{"landing":"land-1"}`); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated retry = %d; want 401", status)
	}
}
