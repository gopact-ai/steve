package delegate

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
)

func TestDeliveryUsesParentsLatestExplicitAddress(t *testing.T) {
	store, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(task.Task{Transport: "feishu", Channel: "console:opaque-native-id", ChatID: "console", AnchorMessage: "old", Member: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Advance(parent.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	child, err := store.Spawn(parent.ID, task.Task{Member: "child", Origin: "delegate:" + parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Advance(child.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	if err = store.SetResult(child.ID, task.Result{Answer: "done"}); err != nil {
		t.Fatal(err)
	}
	if err = store.SetAnchor(parent.ID, channel.Address{Channel: "feishu", Conversation: parent.Channel, Message: "latest"}, "console", "p2p", ""); err != nil {
		t.Fatal(err)
	}
	s := New(store, nil, nil, nil, nil, "", i18n.New(i18n.LocaleZH))
	count := 0
	s.SetDeliverer(func(_ context.Context, d Delivery) error {
		count++
		if d.Transport != "feishu" || d.Anchor != "latest" || d.Conversation != parent.Channel || d.Key != task.DeliveryKey(child.ID) {
			t.Fatalf("delivery=%+v", d)
		}
		return nil
	})
	s.Flush(t.Context(), parent.ID)
	s.Flush(t.Context(), parent.ID)
	if count != 1 {
		t.Fatalf("deliveries=%d", count)
	}
}
