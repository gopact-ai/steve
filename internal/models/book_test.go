package models

import (
	"testing"
	"time"
)

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

// What a harness reported survives a restart, per machine and harness,
// and a later observation replaces an earlier one.
func TestBookRemembersAcrossRestarts(t *testing.T) {
	doc := &memDoc{}
	first := New()
	if err := first.Persist(doc); err != nil {
		t.Fatal(err)
	}
	first.Observe(Observation{Harness: "codex", Current: "GPT 5", Available: []string{"GPT 5", "GPT 5 mini"}, Source: "session"})
	first.Observe(Observation{Node: "node-a", Harness: "codex", Current: "GPT 5 mini", Source: "probe"})
	first.Observe(Observation{Node: "node-a", Harness: "kimi", Source: "probe"})

	second := New()
	if err := second.Persist(doc); err != nil {
		t.Fatal(err)
	}
	hub, ok := second.Get("", "codex")
	if !ok || hub.Current != "GPT 5" || len(hub.Available) != 2 || hub.At.IsZero() {
		t.Fatalf("hub codex = %+v ok=%v", hub, ok)
	}
	if got := second.All(); len(got) != 3 || got[0].Node != "" || got[1].Harness != "codex" || got[2].Harness != "kimi" {
		t.Fatalf("all = %+v", got)
	}
	// A harness that tells nothing is still on record as asked.
	if o, ok := second.Get("node-a", "kimi"); !ok || o.Current != "" || o.Source != "probe" {
		t.Fatalf("kimi = %+v ok=%v", o, ok)
	}
	later := time.Now().Add(time.Minute)
	second.Observe(Observation{Node: "node-a", Harness: "codex", Current: "GPT 5", Source: "session", At: later})
	if o, _ := second.Get("node-a", "codex"); o.Current != "GPT 5" || !o.At.Equal(later) {
		t.Fatalf("replaced = %+v", o)
	}
}
