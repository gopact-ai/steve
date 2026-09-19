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
			store, err := OpenLedger(book, "")
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
			reopened, err := OpenLedger(book, "")
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
