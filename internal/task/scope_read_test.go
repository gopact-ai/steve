package task

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestConversationTaskIDsReadOwnerScopeInOneSnapshot(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	writer, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	put := func(id, transport, channel string) {
		t.Helper()
		raw, err := json.Marshal(taskHead{Task: Task{ID: id, Transport: transport, Channel: channel}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Exec(`INSERT INTO bindings VALUES('task',?,?,'')`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	put("one", "console", "opaque")
	put("other-transport", "feishu", "opaque")
	put("other-chat", "console", "another")
	if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		ids, err := ConversationIDsTx(tx, "console", "opaque")
		if err != nil || !reflect.DeepEqual(ids, []string{"one"}) {
			t.Fatalf("scope: %v %v", ids, err)
		}
		put("two", "console", "opaque")
		ids, err = ConversationIDsTx(tx, "console", "opaque")
		if err != nil || !reflect.DeepEqual(ids, []string{"one"}) {
			t.Fatalf("scope changed inside read boundary: %v %v", ids, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		ids, err := ConversationIDsTx(tx, "console", "opaque")
		if err != nil || !reflect.DeepEqual(ids, []string{"one", "two"}) {
			t.Fatalf("fresh scope: %v %v", ids, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConversationTaskScopeIndexUsesOwnerDecoderAndFailsClosed(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{`, `{"id":"bad","channel":9}`, `{"id":"bad","parent":9}`, `{"id":"bad","attempt_count":-1}`, `{"id":"another"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, book := taskRecordBook(t)
			if _, err := book.DB().Exec(`INSERT INTO bindings VALUES('task','bad',?,'')`, raw); err != nil {
				t.Fatal(err)
			}
			err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
				_, err := ConversationIDsTx(tx, "console", "opaque")
				return err
			})
			if err == nil || !strings.Contains(err.Error(), "bad") {
				t.Fatal("invalid owner record silently disappeared", err)
			}
		})
	}
}

func TestConversationTaskIDsIncludesDelegatedDescendantsAcrossChannels(t *testing.T) {
	_, book := taskRecordBook(t)
	for _, item := range []Task{{ID: "root", Transport: "console", Channel: "opaque"}, {ID: "child", Parent: "root", Transport: "console", Channel: "delegate:root"}, {ID: "grandchild", Parent: "child", Transport: "console", Channel: "delegate:child"}, {ID: "outside", Transport: "feishu", Channel: "opaque"}} {
		if err := book.PutBinding(t.Context(), "task", item.ID, taskHead{Task: item}); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		ids, err := ConversationIDsTx(tx, "console", "opaque")
		if err != nil || !reflect.DeepEqual(ids, []string{"child", "grandchild", "root"}) {
			t.Fatalf("lost descendant native files: %v %v", ids, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConversationTaskScopeRejectsCyclesAndMissingParents(t *testing.T) {
	for _, parent := range []string{"child", "missing"} {
		t.Run(parent, func(t *testing.T) {
			_, book := taskRecordBook(t)
			for _, item := range []Task{{ID: "root", Parent: parent, Transport: "console", Channel: "opaque"}, {ID: "child", Parent: "root"}} {
				if err := book.PutBinding(t.Context(), "task", item.ID, taskHead{Task: item}); err != nil {
					t.Fatal(err)
				}
			}
			if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error { _, err := ConversationIDsTx(tx, "console", "opaque"); return err }); err == nil {
				t.Fatal("invalid task tree was presented as complete")
			}
		})
	}
}
