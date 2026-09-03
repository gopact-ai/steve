package console

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
)

type echo struct{ seen []turn.Request }

func (e *echo) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	e.seen = append(e.seen, req)
	if strings.HasPrefix(req.Input, "/boom") {
		return turn.Result{}, errors.New("no such verb")
	}
	return turn.Result{Title: "T", Text: "echo: " + req.Input}, nil
}

// A console line runs as the owner, in a console conversation, with an
// anchor that is not a Feishu message; replies and milestones are kept
// and published on the change stream.
func TestConsoleActsAsTheOwnerAndKeepsTheExchange(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	h := &echo{}
	s := New(h, "ou_owner", model)
	events, stop := model.Subscribe(context.Background())
	defer stop()

	reply, err := s.Send(context.Background(), "main", "/fleet")
	if err != nil || reply.Text != "echo: /fleet" || reply.Kind != "reply" {
		t.Fatalf("reply = %+v err=%v", reply, err)
	}
	req := h.seen[0]
	if req.ConversationID != "console:main" || req.SenderOpenID != "ou_owner" || !strings.HasPrefix(req.MessageID, AnchorMark) || req.ChatID != ChatID {
		t.Fatalf("request = %+v", req)
	}
	if _, err := s.Send(context.Background(), "console:main", "/boom"); err == nil {
		t.Fatal("an error from the coordinator was swallowed")
	}
	// The exchange is on record: sent, reply, sent, reply(error).
	replies := s.Replies("main")
	if len(replies) != 4 || replies[0].Kind != "sent" || replies[3].Error == "" {
		t.Fatalf("replies = %+v", replies)
	}
	// Milestones from agents and notices for the console land here too.
	id := s.Milestone("web-1", "phase 1 done")
	if !strings.HasPrefix(id, AnchorMark) {
		t.Fatalf("milestone id = %q", id)
	}
	s.Notice(turn.TaskNotice{TaskID: "9", ChatID: ChatID, MessageID: "web-1", Text: "plan finished"})
	replies = s.Replies("main")
	if replies[len(replies)-1].Kind != "notice" || replies[len(replies)-2].Kind != "milestone" {
		t.Fatalf("tail = %+v", replies[len(replies)-2:])
	}
	// Six events were published, console-kinded.
	seen := 0
	for seen < 6 {
		ev := <-events
		if !strings.HasPrefix(ev.Kind, "console.") {
			t.Fatalf("unexpected event %+v", ev)
		}
		seen++
	}
	// Without an owner there is nobody to act as.
	if _, err := New(h, "", model).Send(context.Background(), "main", "hi"); err == nil {
		t.Fatal("a console without an owner acted")
	}
	// Feishu anchors go to Feishu; console anchors stay here.
	var sink recorder
	sender := Sender{Feishu: &sink, Console: s}
	if _, err := sender.ReplyText(context.Background(), "om_real", "to feishu"); err != nil || sink.texts != 1 {
		t.Fatalf("feishu route: err=%v texts=%d", err, sink.texts)
	}
	if _, err := sender.ReplyCard(context.Background(), "web-2", []byte(`{"elements":[{"content":"## card"}]}`)); err != nil || sink.cards != 0 {
		t.Fatalf("console route: err=%v cards=%d", err, sink.cards)
	}
	if last := s.Replies("main"); last[len(last)-1].Text != "## card" {
		t.Fatalf("card text = %q", last[len(last)-1].Text)
	}
}

type recorder struct{ texts, cards int }

func (r *recorder) ReplyCard(context.Context, string, []byte) (string, error) {
	r.cards++
	return "om_c", nil
}
func (r *recorder) ReplyText(context.Context, string, string) (string, error) {
	r.texts++
	return "om_t", nil
}
func (r *recorder) PatchCard(context.Context, string, []byte) error { return nil }
func (r *recorder) DeleteMessage(context.Context, string) error     { return nil }

// memDoc is a durable document that lives for one test.
type memDoc struct {
	raw   []byte
	saved bool
}

func (d *memDoc) Load() ([]byte, bool, error) { return d.raw, d.saved, nil }
func (d *memDoc) Save(raw []byte) error {
	d.raw, d.saved = append([]byte(nil), raw...), true
	return nil
}
func (d *memDoc) Check() error { return nil }

// A restart must not empty the console: the transcript is kept in a
// document and comes back, per conversation, in order, and the page can
// list which conversations exist.
func TestConsoleTranscriptSurvivesARestart(t *testing.T) {
	doc := &memDoc{}
	h := &echo{}
	first := New(h, "ou_owner", nil)
	if err := first.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Send(context.Background(), "main", "/fleet"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Send(context.Background(), "ops", "hello"); err != nil {
		t.Fatal(err)
	}
	if !doc.saved {
		t.Fatal("nothing was written")
	}

	second := New(h, "ou_owner", nil)
	if err := second.Persist(doc); err != nil {
		t.Fatal(err)
	}
	replies := second.Replies("main")
	if len(replies) != 2 || replies[0].Input != "/fleet" || replies[1].Text != "echo: /fleet" {
		t.Fatalf("restored main = %+v", replies)
	}
	if got := second.Conversations(); len(got) != 2 || got[0] != "console:main" || got[1] != "console:ops" {
		t.Fatalf("conversations = %v", got)
	}
	// New lines append after the restored ones and are saved too.
	if _, err := second.Send(context.Background(), "main", "/tasks"); err != nil {
		t.Fatal(err)
	}
	third := New(h, "ou_owner", nil)
	if err := third.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if replies := third.Replies("main"); len(replies) != 4 || replies[2].Input != "/tasks" {
		t.Fatalf("after second restart main = %+v", replies)
	}
}
