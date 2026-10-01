package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/readmodel"
)

type versionedForceControl struct {
	calls    int
	revision uint64
}

func (f *versionedForceControl) ForceStop(_ context.Context, _ string, expectedRevision uint64) error {
	f.calls++
	f.revision = expectedRevision
	return nil
}

func TestForceStopRequiresAnExplicitValidExpectedRevision(t *testing.T) {
	for _, body := range []string{"", "{}", `{"expected_revision":null}`, `{"expected_revision":-1}`, `{"expected_revision":"1"}`, `{"expected_revision":1.5}`, `{"expected_revision":0,"other":true}`, `{"expected_revision":0} {}`} {
		t.Run(body, func(t *testing.T) {
			s := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
			control := &versionedForceControl{}
			s.SetForceStops(control)
			req, _ := http.NewRequest(http.MethodPost, s.URL()+"/console/attempts/original/force-stop", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+testToken)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusBadRequest || control.calls != 0 {
				t.Fatalf("unversioned or malformed confirmation accepted: status=%d calls=%d", res.StatusCode, control.calls)
			}
		})
	}
}

func TestForceStopDispatchesTheConfirmedRevision(t *testing.T) {
	s := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	control := &versionedForceControl{}
	s.SetForceStops(control)
	req, _ := http.NewRequest(http.MethodPost, s.URL()+"/console/attempts/original/force-stop", strings.NewReader(`{"expected_revision":7}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted || control.calls != 1 || control.revision != 7 {
		t.Fatalf("confirmed version lost: status=%d calls=%d revision=%d", res.StatusCode, control.calls, control.revision)
	}
}
