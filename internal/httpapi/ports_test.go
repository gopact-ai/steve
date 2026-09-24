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
	"github.com/gopact-ai/steve/internal/i18n"
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

// ownerRequest sends a request with the owner's token, asking for English,
// and reads the answer, trimmed of the line end that ends every error and
// JSON body.
func ownerRequest(t *testing.T, server *Server, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Accept-Language", "en")
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

// consolePort forwards the methods of consoleapi.Console and nothing else.
type consolePort struct{ consoleapi.Console }

// portConsole answers what the routes below ask of the console.
type portConsole struct{ consoleapi.Console }

func (portConsole) SendSubmission(_ context.Context, submission consoleapi.Submission) (consoleapi.Reply, error) {
	return consoleapi.Reply{ID: submission.Refs[0].ID, Conversation: submission.Conversation}, nil
}
func (portConsole) Submit(_ context.Context, submission consoleapi.Submission) (consoleapi.Exchange, error) {
	return consoleapi.Exchange{ID: submission.Refs[0].ID, Conversation: submission.Conversation}, nil
}
func (portConsole) Questions(conversation string) []consoleapi.PendingQuestion {
	return []consoleapi.PendingQuestion{{ID: "q", Conversation: conversation}}
}
func (portConsole) AnswerQuestion(_ context.Context, id string, answer consoleapi.QuestionAnswer) (consoleapi.PendingQuestion, error) {
	return consoleapi.PendingQuestion{ID: id, Answer: &answer}, nil
}
func (portConsole) InitializeConversation(context.Context, string, string) error { return nil }
func (portConsole) ExchangeConversation(string) (string, bool)                   { return "console:main", true }
func (portConsole) DeleteQueued(string) error                                    { return nil }
func (portConsole) VerbsFor(ctx context.Context) []consoleapi.Verb {
	return []consoleapi.Verb{{Command: "/" + string(i18n.ContextLocale(ctx))}}
}
func (portConsole) SubmissionCapabilities() (materialRefs, interactiveRequests bool) {
	return true, true
}

// pluginsPort forwards the methods of consoleapi.PluginsService and nothing
// else.
type pluginsPort struct{ consoleapi.PluginsService }

// portPlugins answers what the routes below ask of the plugin service.
type portPlugins struct{ consoleapi.PluginsService }

func (portPlugins) PreviewPluginPreset(_ context.Context, id string, _ consoleapi.PluginPresetRequest) (consoleapi.PluginPresetPreview, error) {
	return consoleapi.PluginPresetPreview{Revision: "previewed " + id}, nil
}
func (portPlugins) ApplyPluginPreset(_ context.Context, id string, _ consoleapi.PluginPresetRequest) (consoleapi.PluginPresetPreview, error) {
	return consoleapi.PluginPresetPreview{Revision: "applied " + id}, nil
}
func (portPlugins) PluginUsage(_ context.Context, id string) (consoleapi.PluginUsageView, error) {
	return consoleapi.PluginUsageView{Errors: map[string]string{id: "in use"}}, nil
}
func (portPlugins) RemovePlugin(_ context.Context, id string, _ consoleapi.PluginRemoveRequest) (consoleapi.PluginsView, error) {
	return consoleapi.PluginsView{Revision: "removed " + id}, nil
}
func (portPlugins) ClosePluginRuntime(_ context.Context, id, runtime string) (consoleapi.PluginUsageView, error) {
	return consoleapi.PluginUsageView{Errors: map[string]string{id: "closed " + runtime}}, nil
}

// A route calls what it needs through the service port it holds, so it
// serves a service reached through a port that forwards nothing else. The
// server reads channel history too, as the assembled application's does.
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
		{"POST", "/console/send", `{"input":"hi","refs":[{"id":"m"}]}`, 200, `{"reply":{"id":"m",`},
		{"POST", "/console/queue", `{"input":"hi","refs":[{"id":"m"}]}`, 200, `{"id":"m","conversation":"console:main",`},
		{"GET", "/console/questions?conversation=c", "", 200, `{"questions":[{"id":"q","conversation":"c",`},
		{"POST", "/console/questions/q/answer", `{"decision":"accept"}`, 200, `{"question":{"id":"q",`},
		{"PUT", "/console/conversations/console%3Afresh/initialize", `{"project":"p"}`, 200, `{"ok":true}`},
		{"DELETE", "/console/queue/e", "", 200, `{"ok":true}`},
		{"GET", "/console/verbs", "", 200, `{"verbs":[{"command":"/en",`},
		{"GET", "/console/queue?capabilities=1", "", 200, `{"interactive_requests":true,"material_refs":true,"queue":[],"submission_keys":true}`},
		{"POST", "/console/plugins/installations/tools/presets/preview", `{}`, 200, `{"revision":"previewed tools",`},
		{"POST", "/console/plugins/installations/tools/presets/apply", `{}`, 200, `{"revision":"applied tools",`},
		{"GET", "/console/plugins/installations/tools/usage", "", 200, `"errors":{"tools":"in use"}`},
		{"DELETE", "/console/plugins/installations/tools", `{}`, 200, `"revision":"removed tools"`},
		{"POST", "/console/plugins/installations/tools/runtimes/r/close", "", 200, `"errors":{"tools":"closed r"}`},
	} {
		server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
		server.SetAdmin(adminPort{portAdmin{}})
		server.SetConsole(consolePort{portConsole{}})
		server.SetChannelHistory(channelHistoryStub{})
		server.SetPlugins(pluginsPort{portPlugins{}})
		if res, body := ownerRequest(t, server, route.method, route.path, route.body); res.StatusCode != route.status || !strings.Contains(body, route.want) {
			t.Errorf("%s %s = %d %s, want %d with %s", route.method, route.path, res.StatusCode, body, route.status, route.want)
		}
	}
}
