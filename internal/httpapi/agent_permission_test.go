package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

type permissionReaderAdmin struct {
	consoleapi.Admin
	node, harness string
	calls         int
}

func (a *permissionReaderAdmin) AgentPermission(_ context.Context, node, harness string) (consoleapi.AgentPermission, error) {
	a.calls++
	a.node, a.harness = node, harness
	return consoleapi.AgentPermission{Node: node, Harness: harness, Permission: "auto", Source: consoleapi.AgentPermissionHubHarness, Revision: "fixture-revision"}, nil
}

func TestAgentPermissionRouteIsGuardedReadOnlyAndNonSecret(t *testing.T) {
	a := &permissionReaderAdmin{}
	s := &Server{admin: a, token: "fixture-owner"}
	mux := http.NewServeMux()
	s.agentPermissionRoutes(mux)
	target := "/console/agents/permission?node=worker%2Fid&harness=my-acp"
	denied := httptest.NewRecorder()
	mux.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, target, nil))
	if denied.Code != http.StatusUnauthorized || a.calls != 0 {
		t.Fatal("unauthenticated read reached the coordinator")
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", "Bearer fixture-owner")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK || a.node != "worker/id" || a.harness != "my-acp" || a.calls != 1 {
		t.Fatalf("guarded route: %d %s", response.Code, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("permission observation was cacheable")
	}
	var fact map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &fact); err != nil {
		t.Fatal(err)
	}
	if len(fact) != 5 || fact["permission"] != "auto" || fact["source"] != "hub_harness" {
		t.Fatalf("unexpected permission wire shape: %+v", fact)
	}
	post := httptest.NewRecorder()
	mux.ServeHTTP(post, httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{}`)))
	if post.Code != http.StatusMethodNotAllowed || a.calls != 1 {
		t.Fatal("read-only route accepted a mutation")
	}
}

func TestExpectedAgentPermissionHTTPConflictLeavesAtomicStateUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := &config.Config{Harnesses: map[string]config.Harness{"mock": {Command: "/fixture/never-execute", Permission: "read"}}}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	store := admin.NewConfigStore(cfg)
	a := &admin.Service{NodeName: "local", ConfigStore: store, Path: path, Catalog: catalog}
	// An Admin interface decorator must forward the fact, not lose it as a
	// concrete optional capability at the HTTP boundary.
	s := &Server{admin: adminPort{a}, token: "fixture-owner"}
	mux := http.NewServeMux()
	s.agentPermissionRoutes(mux)
	mux.HandleFunc("POST /console/agents", s.guard(s.consoleAddAgent))
	read := httptest.NewRequest(http.MethodGet, "/console/agents/permission?node=local&harness=mock", nil)
	read.Header.Set("Authorization", "Bearer fixture-owner")
	record := httptest.NewRecorder()
	mux.ServeHTTP(record, read)
	var fact consoleapi.AgentPermission
	if err := json.Unmarshal(record.Body.Bytes(), &fact); err != nil || record.Code != 200 {
		t.Fatalf("read fact = %s %v", record.Body, err)
	}
	if err := store.Update(func(c *config.Config) error {
		h := c.Harnesses["mock"]
		h.Permission = "auto"
		c.Harnesses["mock"] = h
		return nil
	}, func(c *config.Config) error { return config.Save(path, c) }); err != nil {
		t.Fatal(err)
	}
	before := cfg.Clone()
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeCatalog := catalog.List()
	req := consoleapi.AddAgentRequest{ID: "new", Node: "local", Harness: "mock", ExpectedPermission: &fact.Permission, ExpectedPermissionRevision: fact.Revision}
	raw, _ := json.Marshal(req)
	post := httptest.NewRequest(http.MethodPost, "/console/agents", strings.NewReader(string(raw)))
	post.Header.Set("Authorization", "Bearer fixture-owner")
	rejected := httptest.NewRecorder()
	mux.ServeHTTP(rejected, post)
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rejected.Body.Bytes(), &body); err != nil || rejected.Code != 409 || body.Code != "agent_permission_conflict" {
		t.Fatalf("stale permission result = %d %s %v", rejected.Code, rejected.Body, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(disk) || !reflect.DeepEqual(cfg, before) || !reflect.DeepEqual(catalog.List(), beforeCatalog) {
		t.Fatal("HTTP conflict changed file/config/catalog")
	}
	// Explicitly reread and confirm auto; the API asserts it, never sets it.
	current, err := a.AgentPermission(t.Context(), "local", "mock")
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedPermission, req.ExpectedPermissionRevision = &current.Permission, current.Revision
	raw, _ = json.Marshal(req)
	confirmed := httptest.NewRequest(http.MethodPost, "/console/agents", strings.NewReader(string(raw)))
	confirmed.Header.Set("Authorization", "Bearer fixture-owner")
	success := httptest.NewRecorder()
	mux.ServeHTTP(success, confirmed)
	if success.Code != 200 || cfg.Agents["new"].Node != "" || cfg.Harnesses["mock"].Permission != "auto" {
		t.Fatalf("confirmed registration = %d %s", success.Code, success.Body)
	}
}

type permissionConflictAdmin struct {
	consoleapi.Admin
	err error
}

func (a permissionConflictAdmin) AddAgent(context.Context, consoleapi.AddAgentRequest) error {
	return a.err
}

func TestExpectedAgentPermissionHTTPErrorUsesTypedConflictOnly(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("wrapped: %w", consoleapi.ErrAgentPermissionConflict), 409},
		{errors.New(consoleapi.ErrAgentPermissionConflict.Error()), 400},
		{consoleapi.ErrAgentPermissionConfirmationInvalid, 400},
	} {
		s := &Server{admin: permissionConflictAdmin{err: tc.err}}
		record := httptest.NewRecorder()
		s.consoleAddAgent(record, httptest.NewRequest(http.MethodPost, "/console/agents", strings.NewReader(`{"id":"new","harness":"mock"}`)))
		if record.Code != tc.status || strings.Contains(record.Body.String(), `"ok"`) {
			t.Fatalf("error %v became %d %s", tc.err, record.Code, record.Body)
		}
	}
}

func TestAgentPermissionHTTPWithoutConfigurationDoesNotInventFact(t *testing.T) {
	for _, a := range []consoleapi.Admin{nil, &admin.Service{}} {
		s := &Server{admin: a}
		record := httptest.NewRecorder()
		s.consoleAgentPermission(record, httptest.NewRequest(http.MethodGet, "/console/agents/permission?node=missing&harness=unknown", nil))
		if record.Code != 400 && record.Code != 501 {
			t.Fatalf("missing service returned a policy: %d %s", record.Code, record.Body)
		}
	}
}
