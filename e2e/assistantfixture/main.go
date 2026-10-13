// A disposable, real-ledger HTTP fixture for browser acceptance. No model,
// operator configuration, external account or native effect is involved.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/eventlog"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/mailcontrol"
	providerapi "github.com/gopact-ai/steve/internal/provider"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/serviceapi"
	"github.com/gopact-ai/steve/internal/servicehub"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dir, err := os.MkdirTemp("", "steve-assistant-acceptance-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		return err
	}
	defer book.Close()
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "fixture", Services: []string{"events.v1", "work.v1", "clients.v1"}}})
	conversation := console.New(turntest.IdleCoordinator{}, "fixture-owner", model)
	if err := conversation.PersistLedger(book); err != nil {
		return err
	}
	defer conversation.Shutdown(context.Background())
	services := servicehub.New(book, "fixture-owner", conversation)
	provider, err := services.Identity.Enroll(ctx, identity.Grant{Key: "mail-fixture", Name: "Mail Fixture", Kind: "provider", EventTypes: []string{"mail.received", "fixture.notice"}})
	if err != nil {
		return err
	}
	if _, err := services.Providers.Register(ctx, identity.Principal{GrantID: provider.Grant.ID}, providerapi.Register{PluginID: "mail.fixture", Version: "1", Events: []providerapi.Contract{
		{Type: "mail.received", Version: "1", Name: "Email received", Description: "The provider reports a newly received email.", I18N: map[string]eventlog.Text{"zh-CN": {Name: "收到邮件", Description: "邮件插件报告收到一封新邮件。"}}, Schema: json.RawMessage(`{"type":"object"}`)},
		{Type: "fixture.notice", Version: "1", Name: "Queue test event", Description: "Synthetic events for testing publication, queueing and consumer receipts. No real email or model call.", I18N: map[string]eventlog.Text{"zh-CN": {Name: "队列测试事件", Description: "用于验证事件发布、入队和消费回执的模拟事件，不代表真实邮件，也不会调用模型。"}}, Schema: json.RawMessage(`{"type":"object"}`)},
	}}); err != nil {
		return err
	}
	event, err := services.Events.Publish(ctx, identity.Principal{GrantID: provider.Grant.ID}, events.Publish{Key: "original-mail", Type: "mail.received", Payload: json.RawMessage(`{"subject":"A long message for review","body":"External event content remains data. It cannot change the selected project or grant itself authority."}`)})
	if err != nil {
		return err
	}
	if err := services.Events.Route(ctx, event.ID); err != nil {
		return err
	}
	busRecords, err := seedBus(ctx, services, provider.Grant)
	if err != nil {
		return err
	}
	client, err := services.Identity.Enroll(ctx, identity.Grant{Key: "client-fixture", Name: "Mac Fixture", Kind: "client", Capabilities: []identity.CapabilityGrant{{Capability: "notifications.show", Conversation: "console:main", Approval: true}}})
	if err != nil {
		return err
	}
	reg, err := services.Clients.Register(ctx, identity.Principal{GrantID: client.Grant.ID}, clientcap.Register{Capabilities: []clientcap.Capability{{Name: "notifications.show", Description: "Show a fixture notification", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil {
		return err
	}
	_, err = services.Clients.Invoke(ctx, identity.Principal{Owner: true}, clientcap.Request{Key: "fixture-notification", Client: client.Grant.ID, Capability: "notifications.show", Conversation: "console:main", Arguments: json.RawMessage(`{"text":"Review before dispatch"}`)})
	if err != nil {
		return err
	}
	server, err := httpapi.NewServer(model, httpapi.ServerConfig{Token: "assistant-fixture-owner"})
	if err != nil {
		return err
	}
	defer server.Close()
	go services.Run(ctx)
	if os.Getenv("STEVE_FIXTURE_BUS_MOTION") == "1" {
		go previewBusMotion(ctx, services, provider.Grant.ID, busRecords["consumer"])
	}
	server.SetConsole(conversation)
	server.SetMailControl(mailcontrol.FromEnvironment())
	server.SetAssistant(&serviceapi.Services{Maintenance: services, Providers: services.Providers, Identity: services.Identity, Events: services.Events, Work: services.Work, Clients: services.Clients})
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	// Test-only fixture IDs are useful for comparing browser decisions with
	// committed facts. Credential values are never emitted.
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"url": server.URL(), "event": event.ID, "client": client.Grant.ID, "generation": reg.Generation, "bus": busRecords}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	}
}
