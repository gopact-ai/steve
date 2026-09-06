package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type revisionSettingsAdmin struct {
	consoleapi.Admin
	err error
}

func (a revisionSettingsAdmin) SetNodeSettings(context.Context, string, nodewire.Settings) (nodewire.Settings, error) {
	return nodewire.Settings{}, a.err
}
func TestNodeSettingsRevisionConflictMapsToHTTP409(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{fmt.Errorf("remote: %w", nodewire.ErrSettingsRevisionConflict), http.StatusConflict}, {errors.New(nodewire.ErrSettingsRevisionConflict.Error()), http.StatusBadRequest}} {
		s := &Server{admin: revisionSettingsAdmin{err: tc.err}}
		req := httptest.NewRequest(http.MethodPut, "/console/nodes/test/settings", strings.NewReader(`{"revision":"stale"}`))
		req.SetPathValue("name", "test")
		out := httptest.NewRecorder()
		s.nodeSettings(out, req)
		if out.Code != tc.status {
			t.Fatalf("error=%v status=%d want=%d", tc.err, out.Code, tc.status)
		}
	}
}
