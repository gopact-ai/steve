package turn

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/state"
)

func TestRequestLocalesShareExecutionStateWithoutChangingDefault(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := New(catalog, store, nil, nil, time.Minute)
	c.SetCatalog(i18n.New(i18n.LocaleZH))
	var wg sync.WaitGroup
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			locale := i18n.LocaleZH
			if n%2 == 1 {
				locale = i18n.LocaleEN
			}
			conversation := fmt.Sprintf("conversation-%d", n)
			got, err := c.Handle(t.Context(), Request{Locale: string(locale), ConversationID: conversation, Input: "/use worker"})
			if err != nil {
				t.Error(err)
				return
			}
			want := i18n.New(locale).T(i18n.Switched, "worker")
			if got.Text != want {
				t.Errorf("%s: got %q, want %q", locale, got.Text, want)
			}
			if store.Conversation(conversation).ActiveAgent != "worker" {
				t.Error("localized view lost shared state")
			}
		}(n)
	}
	wg.Wait()
	if c.text.Locale() != i18n.LocaleZH {
		t.Fatal("request changed channel default")
	}
	if c.localized(i18n.LocaleEN).coordinatorState != c.coordinatorState {
		t.Fatal("localized request copied runtime state")
	}
}
