package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/readmodel"
)

type forceControl struct {
	calls int
	id    string
}

func (f *forceControl) ForceStop(_ context.Context, id string, expectedRevision uint64) error {
	f.calls++
	f.id = id
	return nil
}
func TestForceStopRequiresOwner(t *testing.T) {
	s := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	for _, token := range []string{"", "wrong", testToken} {
		req, _ := http.NewRequest("POST", s.URL()+"/console/attempts/original/force-stop", strings.NewReader(`{"expected_revision":0}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		want := http.StatusUnauthorized
		if token == testToken {
			want = http.StatusNotImplemented
		}
		if res.StatusCode != want {
			t.Fatalf("token %q returned %d want %d", token, res.StatusCode, want)
		}
	}
}

func TestForceStopOwnerDispatchesOnlyNamedAttempt(t *testing.T) {
	s := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	f := &forceControl{}
	s.SetForceStops(f)
	for _, token := range []string{"wrong", testToken} {
		req, _ := http.NewRequest("POST", s.URL()+"/console/attempts/old-attempt/force-stop", strings.NewReader(`{"expected_revision":0}`))
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Accepted bool `json:"accepted"`
		}
		if token == testToken {
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil || !body.Accepted {
				res.Body.Close()
				t.Fatalf("accepted body=%+v %v", body, err)
			}
		}
		res.Body.Close()
		want := http.StatusUnauthorized
		if token == testToken {
			want = http.StatusAccepted
		}
		if res.StatusCode != want {
			t.Fatalf("status=%d", res.StatusCode)
		}
		if token == testToken && res.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("accepted response content type=%q", res.Header.Get("Content-Type"))
		}
	}
	if f.calls != 1 || f.id != "old-attempt" {
		t.Fatalf("force dispatch=%+v", f)
	}
}
