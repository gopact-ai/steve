package feishu

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestMessageHandlerLiveAccessTransitions(t *testing.T) {
	c := &Channel{}
	calls := 0
	handle := c.messageHandler("ou_bot", false, func(InboundMessage) error {
		calls++
		return nil
	})
	for _, tc := range []struct {
		name      string
		cfg       config.Feishu
		mentioned bool
		dm        bool
		want      bool
	}{
		{"mention required", config.Feishu{GroupPolicy: config.GroupPolicyOpen}, false, false, false},
		{"mentioned allowed", config.Feishu{GroupPolicy: config.GroupPolicyOpen}, true, false, true},
		{"allow unmentioned", config.Feishu{GroupPolicy: config.GroupPolicyOpen, AllowUnmentioned: true}, false, false, true},
		{"disable groups", config.Feishu{GroupPolicy: config.GroupPolicyDisabled, AllowUnmentioned: true}, true, false, false},
		{"allowlist miss", config.Feishu{GroupPolicy: config.GroupPolicyAllowlist, AllowUnmentioned: true}, false, false, false},
		{"allowlist hit", config.Feishu{GroupPolicy: config.GroupPolicyAllowlist, AllowedSenders: []string{"ou_sender"}, AllowUnmentioned: true}, false, false, true},
		{"blocked wins", config.Feishu{GroupPolicy: config.GroupPolicyAllowlist, AllowedSenders: []string{"ou_sender"}, BlockedSenders: []string{"ou_sender"}, AllowUnmentioned: true}, true, false, false},
		{"blocked DM", config.Feishu{GroupPolicy: config.GroupPolicyOpen, BlockedSenders: []string{"ou_sender"}}, false, true, false},
		{"unblock DM with groups disabled", config.Feishu{GroupPolicy: config.GroupPolicyDisabled}, false, true, true},
		{"open ignores allowed list", config.Feishu{GroupPolicy: config.GroupPolicyOpen, AllowedSenders: []string{"someone-else"}, AllowUnmentioned: true}, false, false, true},
		{"mention required again", config.Feishu{GroupPolicy: config.GroupPolicyOpen}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			c.SetAccess(AccessFrom(tc.cfg), tc.cfg.AllowUnmentioned)
			if calls != before {
				t.Fatal("policy change replayed an earlier message")
			}
			event := messageEvent("user", "text", `{"text":"hello"}`)
			if tc.mentioned {
				event.Event.Message.Mentions = []*larkim.MentionEvent{{MentionedType: ptr("bot"), Id: &larkim.UserId{OpenId: ptr("ou_bot")}}}
			}
			if tc.dm {
				event.Event.Message.ChatType = ptr("p2p")
			}
			if err := handle(t.Context(), event); err != nil {
				t.Fatal(err)
			}
			if got := calls == before+1; got != tc.want {
				t.Fatalf("accepted=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestMessageHandlerLiveAccessPreservesFallback(t *testing.T) {
	for _, allow := range []bool{false, true} {
		c := &Channel{access: Access{GroupPolicy: config.GroupPolicyOpen}}
		called := false
		handle := c.messageHandler("ou_bot", allow, func(InboundMessage) error {
			called = true
			return nil
		})
		if err := handle(t.Context(), messageEvent("user", "text", `{"text":"hello"}`)); err != nil {
			t.Fatal(err)
		}
		if called != allow {
			t.Fatalf("fallback accepted=%v, want %v", called, allow)
		}
	}
}

func TestMessageHandlerLiveAccessDoesNotInterruptAcceptance(t *testing.T) {
	c := &Channel{}
	c.SetAccess(Access{GroupPolicy: config.GroupPolicyOpen}, true)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	refused := errors.New("acceptance failure")
	var calls atomic.Int32
	handle := c.messageHandler("ou_bot", false, func(InboundMessage) error {
		if calls.Add(1) != 1 {
			return refused
		}
		close(entered)
		<-release
		return refused
	})
	done := make(chan error, 1)
	go func() { done <- handle(t.Context(), messageEvent("user", "text", `{"text":"first"}`)) }()
	select {
	case <-entered:
	case <-time.After(waitDeadline):
		t.Fatal("message did not enter acceptance")
	}
	updated := make(chan struct{})
	go func() {
		c.SetAccess(Access{GroupPolicy: config.GroupPolicyDisabled}, true)
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(waitDeadline):
		t.Fatal("policy update waited for in-flight acceptance")
	}
	if err := handle(t.Context(), messageEvent("user", "text", `{"text":"next"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("policy update interrupted acceptance: %v", err)
	default:
	}
	release <- struct{}{}
	select {
	case err := <-done:
		if !errors.Is(err, refused) {
			t.Fatalf("acceptance result changed: %v", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("in-flight acceptance failed to finish")
	}
	if calls.Load() != 1 {
		t.Fatal("policy change replayed or admitted another message")
	}
}

func TestMessageHandlerLiveAccessOwnsMaps(t *testing.T) {
	access := Access{GroupPolicy: config.GroupPolicyAllowlist, Allowed: map[string]struct{}{"ou_sender": {}}, Blocked: map[string]struct{}{}}
	c := &Channel{}
	c.SetAccess(access, true)
	delete(access.Allowed, "ou_sender")
	access.Blocked["ou_sender"] = struct{}{}
	called := false
	err := c.messageHandler("ou_bot", false, func(InboundMessage) error {
		called = true
		return nil
	})(t.Context(), messageEvent("user", "text", `{"text":"hello"}`))
	if err != nil || !called {
		t.Fatalf("caller mutation changed published access: called=%v err=%v", called, err)
	}
}

func TestMessageHandlerLiveAccessConcurrentSnapshots(t *testing.T) {
	c := &Channel{}
	c.SetAccess(Access{GroupPolicy: config.GroupPolicyOpen}, false)
	var accepted atomic.Int32
	handle := c.messageHandler("ou_bot", true, func(InboundMessage) error {
		accepted.Add(1)
		return nil
	})
	event := messageEvent("user", "text", `{"text":"hello"}`)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		for range 3000 {
			c.SetAccess(Access{GroupPolicy: config.GroupPolicyDisabled}, true)
			c.SetAccess(Access{GroupPolicy: config.GroupPolicyOpen}, false)
		}
	})
	for range 4 {
		wg.Go(func() {
			<-start
			for range 3000 {
				if err := handle(t.Context(), event); err != nil {
					t.Error(err)
				}
			}
		})
	}
	close(start)
	wg.Wait()
	// Neither snapshot admits this message; a torn policy/mention read can.
	if accepted.Load() != 0 {
		t.Fatalf("mixed snapshots admitted %d messages", accepted.Load())
	}
}

func TestMessageHandlerLiveAccessSynchronizedUpdates(t *testing.T) {
	c := &Channel{}
	var accepted atomic.Int32
	handle := c.messageHandler("ou_bot", false, func(InboundMessage) error {
		accepted.Add(1)
		return nil
	})
	updates, checked := make(chan bool), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for allow := range updates {
			before := accepted.Load()
			if err := handle(t.Context(), messageEvent("user", "text", `{"text":"hello"}`)); err != nil {
				t.Error(err)
			}
			if got := accepted.Load() == before+1; got != allow {
				t.Errorf("completed update not visible: accepted=%v want=%v", got, allow)
			}
			checked <- struct{}{}
		}
	})
	for i := range 200 {
		allow := i%2 == 0
		c.SetAccess(Access{GroupPolicy: config.GroupPolicyOpen}, allow)
		updates <- allow
		<-checked
	}
	close(updates)
	wg.Wait()
}
