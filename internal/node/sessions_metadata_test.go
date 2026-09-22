package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
)

type metadataPromoted struct {
	Items []string `json:"items"`
}
type metadataEnvelope struct{ metadataPromoted }
type metadataPointerEnvelope struct{ *metadataPromoted }
type metadataTextKey struct{ Name string }

func (k *metadataTextKey) MarshalText() ([]byte, error) { return []byte(k.Name), nil }

type metadataMarshaller struct {
	calls *int
	fail  bool
}

func (m metadataMarshaller) MarshalJSON() ([]byte, error) {
	(*m.calls)++
	if m.fail {
		return nil, errors.New("metadata encoder refused")
	}
	return []byte(`{"items":["original"],"integer":9007199254740993}`), nil
}

func metadataQuestion(command string, value any) nodewire.SessionQuestion {
	return nodewire.SessionQuestion{ID: "metadata", CommandID: command, Permission: &permission.Ask{
		Kind: acp.ToolKindOther,
		Options: []acp.PermissionOption{{OptionID: "allow", Name: "allow", Kind: acp.PermissionOptionKindAllowOnce,
			Meta: acp.Meta{"payload": value}}},
	}}
}

func assertCanonicalMetadata(t *testing.T, value any) {
	t.Helper()
	switch v := value.(type) {
	case acp.Meta:
		for _, value := range v {
			assertCanonicalMetadata(t, value)
		}
	case map[string]any:
		for _, value := range v {
			assertCanonicalMetadata(t, value)
		}
	case []any:
		for _, value := range v {
			assertCanonicalMetadata(t, value)
		}
	case nil, bool, string, json.Number:
	default:
		t.Errorf("owner retained dynamic metadata type %T", value)
	}
}

func metadataJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMetadataCommitCanonicalizesPromotedFieldsAndTextMapKeys(t *testing.T) {
	for _, kind := range []string{"private-embedded", "private-embedded-pointer", "nil-embedded-pointer", "text-key"} {
		for _, mode := range []string{"input", "poll", "mutation"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				one := progressSession(t)
				embedded := metadataPromoted{Items: []string{"original"}}
				key := &metadataTextKey{Name: "original"}
				var payload any
				switch kind {
				case "private-embedded":
					payload = metadataEnvelope{embedded}
				case "private-embedded-pointer":
					payload = metadataPointerEnvelope{&embedded}
				case "nil-embedded-pointer":
					payload = metadataPointerEnvelope{}
				case "text-key":
					payload = map[*metadataTextKey]string{key: "value"}
				}
				next := one.copyLocked()
				next.State.Questions = []nodewire.SessionQuestion{metadataQuestion(next.CurrentCommand, payload)}
				want := metadataJSON(t, next.State.Questions)
				if err := one.commitLocked(next); err != nil {
					t.Fatal(err)
				}
				assertCanonicalMetadata(t, one.record.State.Questions[0].Permission.Options[0].Meta)
				if got := metadataJSON(t, one.record.State.Questions); !bytes.Equal(got, want) {
					t.Fatalf("JSON visible fields changed: %s != %s", got, want)
				}
				before := metadataJSON(t, one.record)
				if mode == "input" {
					embedded.Items[0], key.Name = "mutated", "mutated"
					next.State.Questions[0].Permission.Options[0].Meta["payload"] = "mutated"
				} else {
					var state nodewire.SessionState
					if mode == "poll" {
						state = one.state("")
					} else {
						state = one.copyLocked().State
					}
					meta := state.Questions[0].Permission.Options[0].Meta
					// Canonical form is the contract, not the original Go type.
					switch object := meta["payload"].(type) {
					case metadataEnvelope:
						object.Items[0] = "mutated"
					case metadataPointerEnvelope:
						if object.metadataPromoted != nil {
							object.Items[0] = "mutated"
						}
					case map[*metadataTextKey]string:
						for key := range object {
							key.Name = "mutated"
						}
					case map[string]any:
						if items, ok := object["items"].([]any); ok {
							items[0] = "mutated"
						}
						object["original"] = "mutated"
					}
					meta["payload"] = "changed"
				}
				if !bytes.Equal(before, metadataJSON(t, one.record)) {
					t.Fatal("metadata alias changed owner outside commit")
				}
			})
		}
	}
}

func TestMetadataChangedCommitEncodesOnceAndHydratesNumbersExactly(t *testing.T) {
	one := progressSession(t)
	calls := 0
	next := one.copyLocked()
	next.State.Questions = []nodewire.SessionQuestion{metadataQuestion(next.CurrentCommand, metadataMarshaller{calls: &calls})}
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("dynamic metadata encoded %d times during commit", calls)
	}
	// Progress-only commits and reads use only canonical owner values.
	for range 3 {
		_ = one.state("")
		next = one.copyLocked()
		next.State.Progress.Answer += "more"
		if err := one.commitLocked(next); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("unchanged metadata invoked user encoder %d times", calls)
	}
	disk := savedProgress(t, one)
	if got := disk.State.Questions[0].Permission.Options[0].Meta["payload"].(map[string]any)["integer"]; got != json.Number("9007199254740993") {
		t.Fatalf("record hydration rounded large JSON integer: %T %v", got, got)
	}
	next = one.copyLocked()
	next.State.Questions[0] = metadataQuestion(next.CurrentCommand, metadataMarshaller{calls: &calls})
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("changed dynamic metadata not normalized once: %d", calls)
	}
	path := filepath.Join(one.service.server.conf().StateDir, "node-sessions", "sessions.db")
	one.service.closeRecords()
	store, err := openSessionRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	reopened, found, err := store.read(one.record.State.ID, one.record.CurrentCommand)
	if err != nil || !found {
		t.Fatalf("reopen metadata: %t %v", found, err)
	}
	meta := reopened.State.Questions[0].Permission.Options[0].Meta
	assertCanonicalMetadata(t, meta)
	if meta["payload"].(map[string]any)["integer"] != json.Number("9007199254740993") {
		t.Fatal("database reopen changed the original JSON integer")
	}
}

func TestMetadataEqualIngressNeverRetainsExternalMap(t *testing.T) {
	one := progressSession(t)
	first := one.copyLocked()
	first.State.Questions = []nodewire.SessionQuestion{metadataQuestion(first.CurrentCommand, map[string]any{"items": []any{"original"}})}
	if err := one.commitLocked(first); err != nil {
		t.Fatal(err)
	}
	next := one.copyLocked()
	external := acp.Meta{"payload": map[string]any{"items": []any{"original"}}}
	next.State.Questions[0].Permission.Options[0].Meta = external
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	external["payload"].(map[string]any)["items"].([]any)[0] = "mutated"
	if one.record.State.Questions[0].Permission.Options[0].Meta["payload"].(map[string]any)["items"].([]any)[0] != "original" {
		t.Fatal("equal input bypassed metadata ownership")
	}
}

func TestMetadataIngressFailureLeavesOwnerAndNotificationUntouched(t *testing.T) {
	calls := 0
	for _, value := range []any{metadataMarshaller{calls: &calls, fail: true}, make(chan int), math.NaN()} {
		t.Run(reflect.TypeOf(value).String(), func(t *testing.T) {
			one := progressSession(t)
			before := metadataJSON(t, one.record)
			changed := one.changed
			next := one.copyLocked()
			next.State.Questions = []nodewire.SessionQuestion{metadataQuestion(next.CurrentCommand, value)}
			if err := one.commitLocked(next); err == nil {
				t.Fatal("invalid metadata entered the owner")
			}
			if !bytes.Equal(before, metadataJSON(t, one.record)) || one.failure != nil || one.changed != changed {
				t.Fatal("metadata validation failure mutated or quarantined the owner")
			}
			select {
			case <-changed:
				t.Fatal("invalid metadata emitted a transition")
			default:
			}
			if got := savedProgress(t, one); got.State.Sequence != one.record.State.Sequence {
				t.Fatal("invalid metadata changed durable sequence")
			}
		})
	}
}

func TestMetadataHydrationRejectsMalformedMetaInsteadOfACPDefaultNil(t *testing.T) {
	for _, malformed := range []any{"not an object", []any{"array"}, true, 3} {
		t.Run(reflect.TypeOf(malformed).String(), func(t *testing.T) {
			one := progressSession(t)
			next := one.copyLocked()
			next.State.Questions = []nodewire.SessionQuestion{metadataQuestion(next.CurrentCommand, "original")}
			if err := one.commitLocked(next); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(metadataJSON(t, next.State.Questions[0]), &raw); err != nil {
				t.Fatal(err)
			}
			raw["permission"].(map[string]any)["Options"].([]any)[0].(map[string]any)["_meta"] = malformed
			store, err := one.service.recordsStore()
			if err != nil {
				t.Fatal(err)
			}
			// Corrupt only this isolated record, without creating a new authority.
			if _, err := store.db.Exec(`UPDATE session_questions SET question=? WHERE session_id=?`, metadataJSON(t, raw), one.record.State.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.read(one.record.State.ID, next.CurrentCommand); err == nil {
				t.Fatal("malformed persisted metadata silently became nil")
			}
		})
	}
}
