package gateway

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/servicehub"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/work"
	"strings"
	"testing"
)

type plainNative struct {
	addresses []channel.Address
	messages  []channel.Message
	unknown   bool
}

func (p *plainNative) Send(_ context.Context, a channel.Address, m channel.Message) (string, error) {
	if a.Channel != "third" || a.Conversation != "native-private" || a.Message == "" {
		return "", fmt.Errorf("invalid original reply route: %+v", a)
	}
	p.addresses = append(p.addresses, a)
	p.messages = append(p.messages, m)
	if p.unknown && m.Content == "Steve · automation" {
		return "", channel.ErrOutcomeUnknown
	}
	return fmt.Sprintf("native-receipt-%d", len(p.messages)), nil
}
func (*plainNative) Update(context.Context, channel.Address, channel.Message) error {
	return channel.ErrUnsupported
}
func (*plainNative) Recall(context.Context, channel.Address) error                 { return nil }
func (*plainNative) EnrichInput(_ context.Context, in channel.Input) channel.Input { return in }

type nativeWorkCoordinator struct{ fakeProcessor }

func (nativeWorkCoordinator) Context(context.Context, string) (turn.Context, error) {
	return turn.Context{Project: &turn.ContextProject{ID: "p", Bound: true}}, nil
}

func TestThirdChannelWorkRoutesOriginalReceiptsAndNeverRepeatsUnknownAnnouncement(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "known", true: "unknown"}[uncertain], func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			accounts := &identity.Accounts{Book: book}
			cfg := identity.Configuration{ConsoleOwner: "owner"}
			if err := accounts.Bootstrap(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := accounts.Bind(t.Context(), identity.Account{Channel: "third", ID: "native-owner"}, accounts.Owner, 0, true); err != nil {
				t.Fatal(err)
			}
			cfg.Accounts = accounts
			authority, err := identity.NewOwners(cfg)
			if err != nil {
				t.Fatal(err)
			}
			p := &plainNative{unknown: uncertain}
			g := NewChannel("third", nativeWorkCoordinator{})
			g.SetRecoveryLedger(book)
			g.SetAuthority(authority)
			g.BindAdapter(p)
			msg := channel.Input{ConversationID: "native-private", ChatID: "native-private", ChatType: protocol.ChatP2P, MessageID: "original-message", ActorID: "native-owner", Text: "hello", Mentioned: true}
			key := "gateway-input/third/original-message"
			if err := g.acceptInput(t.Context(), key, gatewayInput{Message: msg, Transport: "third"}); err != nil {
				t.Fatal(err)
			}
			runInput := func(key string) {
				t.Helper()
				r, _, err := book.CommandReceipt(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				run, release, err := g.claimGatewayInput(t.Context(), book, r, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if err := run(); err != nil && !uncertain {
					t.Fatal(err)
				}
			}
			runInput(key)
			services := servicehub.New(book, "owner", work.NewRoutes(book, map[string]work.Conversation{"third": g}))
			submitted, err := services.Work.Submit(t.Context(), identity.Principal{Owner: true}, work.Request{Key: "event-1", Conversation: "native-private", Project: "p", Instruction: "summarize the event"})
			if err != nil {
				t.Fatal(err)
			}
			if err := services.Work.Dispatch(t.Context(), submitted.ID); err != nil {
				t.Fatal(err)
			}
			accepted, status, err := services.Work.Status(t.Context(), identity.Principal{Owner: true}, submitted.ID)
			if err != nil || accepted.Exchange == "" {
				t.Fatalf("native admission missing: %+v %+v %v", accepted, status, err)
			}
			runInput(accepted.Exchange)
			replay, err := services.Work.Submit(t.Context(), identity.Principal{Owner: true}, submitted.Request)
			if err != nil || replay.ID != submitted.ID || replay.Exchange != accepted.Exchange {
				t.Fatal("stable event Work acquired another admission")
			}
			runInput(accepted.Exchange)
			_, status, err = services.Work.Status(t.Context(), identity.Principal{Owner: true}, submitted.ID)
			if err != nil {
				t.Fatal(err)
			}
			announcements := 0
			results := 0
			for _, m := range p.messages {
				if m.Content == "Steve · automation" {
					announcements++
				}
				if strings.Contains(m.Content, "summarize the event") {
					results++
				}
			}
			if announcements != 1 {
				t.Fatalf("original automation effect was repeated: %d", announcements)
			}
			if uncertain {
				if status.State != "unknown" || results != 0 {
					t.Fatalf("unknown announcement started intelligence or claimed delivery: %+v results=%d", status, results)
				}
				if err := services.Pump(t.Context()); err != nil {
					t.Fatal(err)
				}
				inbox, err := services.Events.InboxEntries(t.Context(), "")
				if err != nil || len(inbox.Items) != 1 {
					t.Fatalf("uncertain effect has no review entry: %+v %v", inbox, err)
				}
			} else if status.State != "done" || results != 1 {
				t.Fatalf("native Work lost one retained result: %+v results=%d", status, results)
			}
			history, err := NewChannelHistory(book).Read(t.Context(), "native-private", "", 20)
			if err != nil || history.Conversation.Transport != "third" {
				t.Fatalf("native history fell back to another channel: %+v %v", history.Conversation, err)
			}
		})
	}
}
