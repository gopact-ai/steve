package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/provider"
	"github.com/gopact-ai/steve/internal/serviceapi"
)

func TestProviderCredentialHTTPRecoveryPreservesSourceAndAuthorityBoundaries(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	ids := &identity.Service{Book: book, OwnerID: "owner"}
	providers := &provider.Service{Book: book, Identity: ids}
	bus := &events.Service{Book: book, Identity: ids}
	s := &Server{token: "fixture-owner", assistant: &serviceapi.Services{Identity: ids, Providers: providers, Events: bus}}
	mux := http.NewServeMux()
	s.assistantRoutes(mux)
	call := func(method, path, credential, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Accept-Language", "en")
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, req)
		if out.Code != want {
			t.Fatalf("%s %s: status=%d want=%d", method, path, out.Code, want)
		}
		return out
	}
	enrollment := call(http.MethodPost, "/console/connections", "fixture-owner", `{"idempotency_key":"mail","name":"Mail","kind":"provider","event_types":["mail.*"]}`, 200)
	var initial identity.Enrollment
	if err := json.Unmarshal(enrollment.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	body := `{"key":"mailbox/message-123","type":"mail.received","payload":{"subject":"Hello"}}`
	first := call(http.MethodPost, "/services/events", initial.Token, body, 200)
	var original events.Event
	if err := json.Unmarshal(first.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	path := "/console/connections/" + initial.Grant.ID + "/credential"
	request := fmt.Sprintf(`{"revision":%d,"idempotency_key":"response-lost"}`, initial.Grant.CredentialRevision)
	call(http.MethodPost, path, initial.Token, request, 401)
	rotated := call(http.MethodPost, path, "fixture-owner", request, 200)
	if rotated.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential recovery response was cacheable")
	}
	var replacement identity.Enrollment
	if err := json.Unmarshal(rotated.Body.Bytes(), &replacement); err != nil || replacement.Token == "" {
		t.Fatal("credential recovery did not issue a replacement bearer")
	}
	call(http.MethodPost, "/services/events", initial.Token, body, 401)
	duplicate := call(http.MethodPost, "/services/events", replacement.Token, body, 200)
	var replayed events.Event
	if err := json.Unmarshal(duplicate.Body.Bytes(), &replayed); err != nil || replayed.ID != original.ID || replayed.Source != original.Source {
		t.Fatal("credential recovery turned a repeated mailbox message into a new event")
	}
	retry := call(http.MethodPost, path, "fixture-owner", request, 200)
	var lost identity.Enrollment
	if err := json.Unmarshal(retry.Body.Bytes(), &lost); err != nil || lost.Token != "" {
		t.Fatal("idempotent rotation revealed or replaced the existing credential")
	}
	call(http.MethodPost, path, "fixture-owner", fmt.Sprintf(`{"revision":%d,"idempotency_key":"stale"}`, initial.Grant.CredentialRevision), 409)
	registration := `{"generation":0,"plugin_id":"mail.host","version":"1.0.0","events":[{"type":"mail.received","version":"1","name":"Email received","description":"New message","i18n":{"zh-CN":{"name":"收到邮件","description":"报告新邮件到达。"}},"schema":{"type":"object"}}]}`
	call(http.MethodPost, "/services/provider/register", "fixture-owner", registration, 401)
	registered := call(http.MethodPost, "/services/provider/register", replacement.Token, registration, 200)
	var declaration provider.Registration
	if err := json.Unmarshal(registered.Body.Bytes(), &declaration); err != nil || declaration.ID != initial.Grant.ID || declaration.Events[0].Digest == "" {
		t.Fatal("provider declaration was not attached to the logical connection")
	}
	call(http.MethodPost, "/services/provider/heartbeat", replacement.Token, `{"generation":0}`, 409)
	call(http.MethodPost, "/services/provider/heartbeat", replacement.Token, `{"generation":1}`, 200)
	call(http.MethodGet, "/services/provider", replacement.Token, "", 200)
	call(http.MethodGet, "/console/event-providers", replacement.Token, "", 401)
	call(http.MethodGet, "/console/event-providers", "fixture-owner", "", 200)
	call(http.MethodGet, "/console/event-activity", replacement.Token, "", 401)
	call(http.MethodGet, "/console/event-activity", "fixture-owner", "", 200)
	call(http.MethodPost, "/services/provider/register", replacement.Token, `{"generation":1,"plugin_id":"mail.host","version":"1.0.1","events":[{"type":"mail.received","version":"1","schema":{"type":"object","required":["subject"]}}]}`, 409)
	versionedBody := `{"key":"mailbox/versioned-123","type":"mail.received","schema_version":"1","payload":{"subject":"Hello"}}`
	versioned := call(http.MethodPost, "/services/events", replacement.Token, versionedBody, 200)
	var frozen events.Event
	if err := json.Unmarshal(versioned.Body.Bytes(), &frozen); err != nil || frozen.Contract == nil || frozen.Contract.Version != "1" || frozen.Contract.Name != "Email received" || frozen.Contract.I18N["zh-CN"].Description != "报告新邮件到达。" {
		t.Fatal("versioned event did not freeze its declared contract")
	}
	call(http.MethodPost, "/services/provider/register", replacement.Token, `{"generation":1,"plugin_id":"mail.host","version":"2.0.0","events":[{"type":"mail.received","version":"2","description":"Required subject","schema":{"type":"object","required":["subject"]}}]}`, 200)
	versionedRetry := call(http.MethodPost, "/services/events", replacement.Token, versionedBody, 200)
	var retained events.Event
	if err := json.Unmarshal(versionedRetry.Body.Bytes(), &retained); err != nil || retained.ID != frozen.ID || retained.Contract == nil || retained.Contract.Digest != frozen.Contract.Digest || retained.Contract.Name != frozen.Contract.Name || retained.Contract.I18N["zh-CN"] != frozen.Contract.I18N["zh-CN"] {
		t.Fatal("schema upgrade rewrote a repeated publisher's original receipt")
	}
	journalResponse := call(http.MethodGet, "/console/business-events", "fixture-owner", "", 200)
	var journal durable.Page[events.JournalEntry]
	if err := json.Unmarshal(journalResponse.Body.Bytes(), &journal); err != nil {
		t.Fatal(err)
	}
	foundDefinition := false
	for _, item := range journal.Items {
		if item.ID == frozen.ID {
			foundDefinition = item.Contract != nil && item.Contract.Name == "Email received" && item.Contract.I18N["zh-CN"].Name == "收到邮件"
		}
	}
	if !foundDefinition || strings.Contains(journalResponse.Body.String(), `"schema":`) {
		t.Fatal("journal omitted the frozen definition or eagerly returned its schema")
	}
	call(http.MethodPost, "/services/events", replacement.Token, `{"key":"mailbox/versioned-123","type":"mail.received","schema_version":"2","payload":{"subject":"Hello"}}`, 409)
	call(http.MethodPost, "/services/events", replacement.Token, `{"key":"mailbox/new-old-version","type":"mail.received","schema_version":"1","payload":{"subject":"Hello"}}`, 400)
	call(http.MethodPut, "/console/connections/"+initial.Grant.ID+"/scope", "fixture-owner", `{"revision":1,"name":"Mail","event_types":["mail.received"]}`, 200)
	call(http.MethodPut, "/console/connections/"+initial.Grant.ID+"/scope", "fixture-owner", `{"revision":1,"name":"Stale mail","event_types":["mail.*"]}`, 409)
	call(http.MethodPost, "/services/events", replacement.Token, `{"key":"new","type":"mail.sent","payload":{}}`, 403)
	call(http.MethodGet, "/console/connection-audit", "fixture-owner", "", 200)
	client, err := ids.Enroll(t.Context(), identity.Grant{Key: "mac", Name: "Mac", Kind: "client"})
	if err != nil {
		t.Fatal(err)
	}
	call(http.MethodPost, "/services/provider/register", client.Token, registration, 403)
	call(http.MethodDelete, "/console/connections/"+initial.Grant.ID, "fixture-owner", "", 200)
	call(http.MethodPost, "/services/provider/heartbeat", replacement.Token, `{"generation":1}`, 401)
	call(http.MethodPost, path, "fixture-owner", fmt.Sprintf(`{"revision":%d,"idempotency_key":"retired"}`, replacement.Grant.CredentialRevision), 403)
}
