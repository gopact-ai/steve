package console

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
)

func milestoneFixture(t *testing.T) (*Service, *ledger.Ledger, channel.Address) {
	t.Helper()
	l, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	s := New(&echo{}, "owner", readmodel.New(readmodel.Sources{}))
	if err := s.Persist(l.Document("console")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Send(context.Background(), "side", "do some work")
	if err != nil {
		t.Fatal(err)
	}
	return s, l, channel.Address{Channel: "console", Conversation: "console:side", Message: AnchorMark + r.ExchangeID}
}

func assertMilestonePersisted(t *testing.T, s *Service, l *ledger.Ledger) {
	t.Helper()
	reloaded := New(&echo{}, "owner", nil)
	if err := reloaded.Persist(l.Document("console")); err != nil {
		t.Fatal(err)
	}
	for _, conversation := range []string{"side", "main"} {
		if got, want := reloaded.Replies(conversation), s.Replies(conversation); !reflect.DeepEqual(got, want) {
			t.Fatalf("reloaded %s replies = %+v, want %+v", conversation, got, want)
		}
	}
}

func nextMilestoneEvent(t *testing.T, events <-chan readmodel.Event) readmodel.Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(time.Second):
		t.Fatal("missing console milestone event")
		return readmodel.Event{}
	}
}

func TestConsoleMilestoneSendUpdateRecall(t *testing.T) {
	s, l, address := milestoneFixture(t)
	sender := MessageSender{Console: s}
	events, stop := s.model.Subscribe(context.Background())
	defer stop()
	message := channel.Message{Content: "First milestone", Format: "markdown", Attribution: "builder · test"}
	id, err := sender.Send(context.Background(), address, message)
	if err != nil {
		t.Fatal(err)
	}
	replies := s.Replies("side")
	if len(replies) != 3 || id == "" || replies[2].ID != id || replies[2].Text != message.Content || replies[2].Format != message.Format || replies[2].Title != message.Attribution || replies[2].ExchangeID == "" {
		t.Fatalf("milestone = %+v, receipt = %q", replies, id)
	}
	if ev := nextMilestoneEvent(t, events); ev.Kind != "console.milestone" || ev.ReplyID != id || ev.Conversation != address.Conversation || ev.Text != message.Content || ev.Format != message.Format || ev.Title != message.Attribution {
		t.Fatalf("send event = %+v", ev)
	}
	assertMilestonePersisted(t, s, l)

	address.Message = id
	message.Content, message.Format = "**Literal milestone**\n[not a link](https://example.test)", "text"
	if err := sender.Update(context.Background(), address, message); err != nil {
		t.Fatal(err)
	}
	updated := s.Replies("side")
	if len(updated) != 3 || updated[2].ID != id || updated[2].Text != message.Content || updated[2].Format != message.Format || !updated[2].At.Equal(replies[2].At) {
		t.Fatalf("updated milestone = %+v", updated)
	}
	if ev := nextMilestoneEvent(t, events); ev.Kind != "console.milestone" || ev.ReplyID != id || ev.Text != message.Content || ev.Format != message.Format {
		t.Fatalf("update event = %+v", ev)
	}
	assertMilestonePersisted(t, s, l)

	if err := sender.Recall(context.Background(), address); err != nil {
		t.Fatal(err)
	}
	if got := s.Replies("side"); len(got) != 2 || got[0].Kind != "sent" || got[1].Kind != "reply" {
		t.Fatalf("recall changed other lines: %+v", got)
	}
	if ev := nextMilestoneEvent(t, events); ev.Kind != "console.recalled" || ev.ReplyID != id || ev.Conversation != address.Conversation {
		t.Fatalf("recall event = %+v", ev)
	}
	assertMilestonePersisted(t, s, l)
	if got := s.Replies("main"); len(got) != 0 {
		t.Fatalf("milestone leaked to main: %+v", got)
	}
	if err := sender.Recall(context.Background(), address); err == nil {
		t.Fatal("recalling an absent milestone succeeded")
	}
}

func TestConsoleMilestoneRejectsInvalidDestinations(t *testing.T) {
	s, l, valid := milestoneFixture(t)
	sender := MessageSender{Console: s}
	message := channel.Message{Content: "progress", Format: "markdown"}
	id, err := sender.Send(context.Background(), valid, message)
	if err != nil {
		t.Fatal(err)
	}
	events, stop := s.model.Subscribe(context.Background())
	defer stop()
	before := s.Replies("side")
	for _, invalid := range []channel.Address{
		{Channel: "feishu", Conversation: valid.Conversation, Message: valid.Message},
		{Channel: "console", Conversation: "console:main", Message: valid.Message},
		{Channel: "console", Conversation: "", Message: valid.Message},
		{Channel: "console", Conversation: valid.Conversation, Message: "web-unknown"},
		{Channel: "console", Conversation: valid.Conversation, Message: before[1].ExchangeID},
	} {
		if _, err := sender.Send(context.Background(), invalid, message); err == nil {
			t.Errorf("send accepted invalid destination %+v", invalid)
		}
	}
	for _, invalid := range []channel.Address{
		{Channel: "feishu", Conversation: valid.Conversation, Message: id},
		{Channel: "console", Conversation: "console:main", Message: id},
		{Channel: "console", Conversation: valid.Conversation, Message: valid.Message},
		{Channel: "console", Conversation: valid.Conversation, Message: before[0].ID},
		{Channel: "console", Conversation: valid.Conversation, Message: before[1].ID},
		{Channel: "console", Conversation: valid.Conversation, Message: "missing"},
	} {
		if err := sender.Update(context.Background(), invalid, message); err == nil {
			t.Errorf("update accepted invalid destination %+v", invalid)
		}
		if err := sender.Recall(context.Background(), invalid); err == nil {
			t.Errorf("recall accepted invalid destination %+v", invalid)
		}
	}
	if !reflect.DeepEqual(s.Replies("side"), before) || len(s.Replies("main")) != 0 {
		t.Fatal("invalid destination changed a conversation")
	}
	assertMilestonePersisted(t, s, l)
	select {
	case ev := <-events:
		t.Fatalf("rejected operation published %+v", ev)
	default:
	}
}

func TestConsoleMilestoneSaveFailureKeepsTranscriptAndEvents(t *testing.T) {
	s, l, address := milestoneFixture(t)
	sender := MessageSender{Console: s}
	message := channel.Message{Content: "saved", Format: "markdown"}
	id, err := sender.Send(context.Background(), address, message)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Replies("side")
	events, stop := s.model.Subscribe(context.Background())
	defer stop()
	if _, err := l.DB().Exec("PRAGMA query_only = ON"); err != nil {
		t.Fatal(err)
	}
	message.Content = "must not appear"
	if _, err := sender.Send(context.Background(), address, message); err == nil {
		t.Fatal("send swallowed persistence error")
	}
	address.Message = id
	if err := sender.Update(context.Background(), address, message); err == nil {
		t.Fatal("update swallowed persistence error")
	}
	if err := sender.Recall(context.Background(), address); err == nil {
		t.Fatal("recall swallowed persistence error")
	}
	if !reflect.DeepEqual(s.Replies("side"), before) {
		t.Fatal("failed persistence changed live transcript")
	}
	select {
	case ev := <-events:
		t.Fatalf("failed persistence published %+v", ev)
	default:
	}
	if _, err := l.DB().Exec("PRAGMA query_only = OFF"); err != nil {
		t.Fatal(err)
	}
	assertMilestonePersisted(t, s, l)
}
