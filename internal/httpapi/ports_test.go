package httpapi

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// A server holds no admin, console or plugins service until one is set, and
// e2e/mesh serves a console alone. Each route that needs a missing service
// answers that it is off, and the verb list is empty.
func TestCapabilityRoutesWithoutTheirService(t *testing.T) {
	server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	const (
		plain       = "Content-Type: text/plain; charset=utf-8; X-Content-Type-Options: nosniff"
		plainStored = "Cache-Control: no-store; " + plain
		jsonStored  = "Cache-Control: no-store; Content-Type: application/json"
	)
	for _, route := range []struct {
		method, path, body string
		status             int
		header, want       string
	}{
		{"GET", "/console/materials?project=p", "", 501, plain, "materials are not enabled"},
		{"POST", "/console/materials/capture", `{}`, 501, plain, "materials are not enabled"},
		{"POST", "/console/materials/upload?project=p&name=n", "data", 501, plain, "materials are not enabled"},
		{"POST", "/console/materials/resolve", `{}`, 501, plain, "materials are not enabled"},
		{"GET", "/console/materials/m?project=p", "", 501, plain, "materials are not enabled"},
		{"GET", "/console/materials/m/content?project=p", "", 501, plain, "materials are not enabled"},
		{"GET", "/console/annotations?project=p", "", 501, plain, "materials are not enabled"},
		{"PUT", "/console/annotations/a", `{}`, 501, plain, "materials are not enabled"},
		{"GET", "/console/nodes/worker/native-history", "", 501, jsonStored, `{"error":"native session history format is unsupported"}`},
		{"POST", "/console/nodes/worker/native-history", `{}`, 501, jsonStored, `{"error":"native session history format is unsupported"}`},
		{"GET", "/console/nodes/worker/agents", "", 501, jsonStored, `{"error":"当前服务不支持节点工具登记"}`},
		{"POST", "/console/nodes/worker/agents", `{}`, 501, jsonStored, `{"error":"当前服务不支持节点工具登记"}`},
		{"GET", "/console/versions", "", 501, plain, "version discovery is unavailable"},
		{"GET", "/console/attempts?task_id=1", "", 501, plainStored, "native attempt queries are not wired"},
		{"GET", "/console/tasks/1/attempts", "", 501, plainStored, "native attempt queries are not wired"},
		{"PUT", "/console/conversations/console%3Afresh/initialize", `{"project":"p"}`, 501, plain, "conversation initialization is not supported"},
		{"GET", "/console/questions", "", 501, plain, "console interactions are not enabled"},
		{"POST", "/console/questions/q/answer", `{"decision":"accept"}`, 501, plain, "console interactions are not enabled"},
		{"POST", "/console/send", `{"input":"hello"}`, 501, plain, "the console is not enabled on this gateway"},
		{"POST", "/console/queue", `{"input":"hello"}`, 501, plain, "the console is not enabled on this gateway"},
		{"GET", "/console/queue?capabilities=1", "", 501, plainStored, "the console is not enabled on this gateway"},
		{"DELETE", "/console/queue/1", "", 501, plain, "the console is not enabled on this gateway"},
		{"GET", "/console/verbs", "", 200, "Content-Type: application/json", `{"verbs":[]}`},
		{"POST", "/console/plugins/installations/tools/presets/preview", `{}`, 501, plain, "plugin presets are unavailable"},
		{"POST", "/console/plugins/installations/tools/presets/apply", `{}`, 501, plain, "plugin presets are unavailable"},
		{"GET", "/console/plugins/installations/tools/usage", "", 501, plain, "plugin removal is unavailable"},
		{"DELETE", "/console/plugins/installations/tools", `{}`, 501, plain, "plugin removal is unavailable"},
		{"POST", "/console/plugins/installations/tools/runtimes/r/close", "", 501, plain, "plugin runtime close unavailable"},
	} {
		res, body := ownerRequest(t, server, route.method, route.path, route.body)
		if header := responseHeader(res.Header); res.StatusCode != route.status || header != route.header || body != route.want {
			t.Errorf("%s %s = %d [%s] %s, want %d [%s] %s", route.method, route.path, res.StatusCode, header, body, route.status, route.header, route.want)
		}
	}
}

// ownerRequest sends a request with the owner's token and reads the answer,
// trimmed of the line end that ends every error and JSON body.
func ownerRequest(t *testing.T, server *Server, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return res, strings.TrimSpace(string(raw))
}

// responseHeader renders what a handler chose to send, leaving out what
// the transport adds on its own.
func responseHeader(header http.Header) string {
	var fields []string
	for key, values := range header {
		if key != "Date" && key != "Content-Length" {
			fields = append(fields, key+": "+strings.Join(values, ", "))
		}
	}
	sort.Strings(fields)
	return strings.Join(fields, "; ")
}

// adminPort forwards the methods of consoleapi.Admin and nothing else, as a
// decorator around the admin service would.
type adminPort struct{ consoleapi.Admin }

// portAdmin answers what the routes below ask of the admin.
type portAdmin struct{ consoleapi.Admin }

func (portAdmin) ListMaterials(_ context.Context, project string) ([]material.Material, error) {
	return []material.Material{{ID: "m", Project: project}}, nil
}
func (portAdmin) NativeHistory(_ context.Context, node string, source nativehistory.Source) ([]nativehistory.Entry, error) {
	return []nativehistory.Entry{{NativeID: node, Harness: source.Harness}}, nil
}
func (portAdmin) NodeAgents(_ context.Context, node string) (agenttools.Discovery, error) {
	return agenttools.Discovery{Revision: node}, nil
}
func (portAdmin) Versions(context.Context) (consoleapi.Versions, error) {
	return consoleapi.Versions{Hub: "v"}, nil
}
func (portAdmin) QueryAttempts(_ context.Context, query consoleapi.AttemptHistoryQuery) (consoleapi.AttemptHistoryPage, error) {
	return consoleapi.AttemptHistoryPage{NextCursor: query.TaskID}, nil
}

// A route calls what it needs through the service port it holds, so it
// serves a service reached through a port that forwards nothing else.
func TestCapabilityRoutesNeedNothingOutsideTheirPort(t *testing.T) {
	for _, route := range []struct {
		method, path, body string
		status             int
		want               string
	}{
		{"GET", "/console/materials?project=p", "", 200, `{"materials":[{"id":"m","project":"p",`},
		{"GET", "/console/nodes/worker/native-history?harness=dsh", "", 200, `{"entries":[{"native_id":"worker","harness":"dsh",`},
		{"GET", "/console/nodes/worker/agents", "", 200, `{"revision":"worker","agents":null}`},
		{"GET", "/console/versions", "", 200, `"hub":"v"`},
		{"GET", "/console/attempts?task_id=t", "", 200, `{"items":null,"next_cursor":"t"}`},
		{"GET", "/console/tasks/t/attempts", "", 200, `{"items":null,"next_cursor":"t"}`},
	} {
		server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
		server.SetAdmin(adminPort{portAdmin{}})
		if res, body := ownerRequest(t, server, route.method, route.path, route.body); res.StatusCode != route.status || !strings.Contains(body, route.want) {
			t.Errorf("%s %s = %d %s, want %d with %s", route.method, route.path, res.StatusCode, body, route.status, route.want)
		}
	}
}
