package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

type abandonCapture struct {
	calls    int
	id       string
	revision uint64
}

func (c *abandonCapture) Abandon(_ context.Context, id string, revision uint64) (consoleapi.Abandonment, error) {
	c.calls++
	c.id, c.revision = id, revision
	return consoleapi.Abandonment{Accepted: true, Pending: true}, nil
}

func TestAbandonEndpointRequiresOwnerAndAnExplicitRevision(t *testing.T) {
	s := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	control := &abandonCapture{}
	s.SetAbandons(control)
	for _, test := range []struct {
		token, body string
		status      int
	}{
		{"", `{"expected_revision":1}`, http.StatusUnauthorized},
		{testToken, `{}`, http.StatusBadRequest},
		{testToken, `{"expected_revision":null}`, http.StatusBadRequest},
		{testToken, `{"expected_revision":-1}`, http.StatusBadRequest},
		{testToken, `{"expected_revision":1,"owner":"other"}`, http.StatusBadRequest},
		{testToken, `{"expected_revision":1} {}`, http.StatusBadRequest},
		{testToken, `{"expected_revision":7}`, http.StatusAccepted},
	} {
		req, _ := http.NewRequest("POST", s.URL()+"/console/attempts/original/abandon", strings.NewReader(test.body))
		if test.token != "" {
			req.Header.Set("Authorization", "Bearer "+test.token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != test.status {
			t.Fatalf("body=%s status=%d want=%d response=%s", test.body, res.StatusCode, test.status, body)
		}
		if test.status == http.StatusAccepted {
			var got consoleapi.Abandonment
			if err := json.Unmarshal(body, &got); err != nil || !got.Accepted || !got.Pending || res.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("accepted is not explicit about pending projection: %s", body)
			}
		}
	}
	if control.calls != 1 || control.id != "original" || control.revision != 7 {
		t.Fatalf("abandon dispatch=%+v", control)
	}
}
