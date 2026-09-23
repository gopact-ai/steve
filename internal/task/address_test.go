package task

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestTaskAddressSurvivesReloadAndCannotRebind(t *testing.T) {
	for _, transport := range []string{"console", "feishu"} {
		t.Run(transport, func(t *testing.T) {
			book, err := ledger.Open(filepath.Join(t.TempDir(), "book"), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			store, err := OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			root, err := store.Create(Task{Transport: transport, Channel: "conversation", Member: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			want := channel.Address{Channel: transport, Conversation: "conversation", Message: "new-message"}
			if err := store.SetAnchor(root.ID, want, "native-chat", "group", "card"); err != nil {
				t.Fatal(err)
			}
			before, _ := store.Get(root.ID)
			for _, wrong := range []channel.Address{{Channel: "another", Conversation: want.Conversation, Message: "bad"}, {Channel: transport, Conversation: "other", Message: "bad"}} {
				if err := store.SetAnchor(root.ID, wrong, "wrong", "p2p", "wrong"); err == nil {
					t.Fatal("rebound task address")
				}
				after, _ := store.Get(root.ID)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed rebind mutated task")
				}
			}
			child, err := store.Spawn(root.ID, Task{Member: "child"})
			if err != nil {
				t.Fatal(err)
			}
			if child.Address() != want {
				t.Fatalf("child address = %+v", child.Address())
			}
			reopened, err := OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := reopened.Get(root.ID)
			if got.Address() != want || got.Channel != "conversation" {
				t.Fatalf("address = %+v", got)
			}
		})
	}
}

func TestTurnAdmissionPersistsAnchorWithItsCharge(t *testing.T) {
	store, _ := newStore(t)
	root, err := store.Create(Task{Transport: "console", Channel: "chat", AnchorMessage: "old", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(root.ID)
	input := TurnInput{Address: channel.Address{Channel: "feishu", Conversation: "chat", Message: "wrong"}}
	if _, err := store.BeginTurn(root.ID, "worker", "node", input); err == nil {
		t.Fatal("cross-transport turn admitted")
	}
	after, _ := store.Get(root.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused anchor charged a turn")
	}
	input.Address.Channel = "console"
	input.Address.Message = "new"
	doc := &metaDocument{Doc: store.doc, fail: true}
	store.doc = doc
	if _, err := store.BeginTurn(root.ID, "worker", "node", input); err == nil {
		t.Fatal("failed persistence admitted a turn")
	}
	after, _ = store.Get(root.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed save partially charged or reanchored")
	}
	doc.fail = false
	if _, err := store.BeginTurn(root.ID, "worker", "node", input); err != nil {
		t.Fatal(err)
	}
	reloaded, err := openWith(store.doc)
	if err != nil {
		t.Fatal(err)
	}
	after, _ = reloaded.Get(root.ID)
	if after.AnchorMessage != "new" || after.Budget.Turns != 1 || len(after.Attempts) != 1 {
		t.Fatalf("admitted turn=%+v", after)
	}
}
