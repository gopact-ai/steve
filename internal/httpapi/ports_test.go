package httpapi

import (
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

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
		req, err := http.NewRequest(route.method, server.URL()+route.path, strings.NewReader(route.body))
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
		if header, body := responseHeader(res.Header), strings.TrimSpace(string(raw)); res.StatusCode != route.status || header != route.header || body != route.want {
			t.Errorf("%s %s = %d [%s] %s, want %d [%s] %s", route.method, route.path, res.StatusCode, header, body, route.status, route.header, route.want)
		}
	}
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
