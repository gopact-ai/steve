package httpapi

import (
	"context"
	"github.com/gopact-ai/steve/internal/readmodel"
	"io"
	"net/http"
	"strings"
	"testing"
)

type forceControl struct {
	calls int
	id    string
}

func (f *forceControl) ForceStop(_ context.Context, id string) error {
	f.calls++
	f.id = id
	return nil
}
func TestForceStopRequiresOwner(t *testing.T) {
	s := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	for _, token := range []string{"", "wrong", testToken} {
		req, _ := http.NewRequest("POST", s.URL()+"/console/attempts/original/force-stop", strings.NewReader("{}"))
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
		req, _ := http.NewRequest("POST", s.URL()+"/console/attempts/old-attempt/force-stop", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		want := http.StatusUnauthorized
		if token == testToken {
			want = http.StatusAccepted
		}
		if res.StatusCode != want {
			t.Fatalf("status=%d", res.StatusCode)
		}
	}
	if f.calls != 1 || f.id != "old-attempt" {
		t.Fatalf("force dispatch=%+v", f)
	}
}
