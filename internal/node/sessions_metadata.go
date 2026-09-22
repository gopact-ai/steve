package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Dynamic Go metadata becomes JSON-owned values before it enters live state.
// This preserves the wire contract, not Go types or private memory graphs.
// encoding/json owns promoted fields, TextMarshaler keys and cycle rejection;
// no reflection clone attempts to reproduce those serialization rules.
func canonicalSessionMetadata(meta acp.Meta) (acp.Meta, error) {
	raw, err := sessionRecordJSON(meta)
	if err != nil {
		return nil, err
	}
	// Do not invoke ACP Meta.UnmarshalJSON, which defaults malformed metadata
	// to nil. An invalid value at this durable boundary must be an error.
	var out map[string]any
	if err := decodeSessionMetadataJSON(raw, &out); err != nil {
		return nil, err
	}
	return acp.Meta(out), nil
}

// UseNumber applies at both ingress and persisted question hydration. Otherwise
// a restart could round metadata integers that exceeded float64 precision.
func decodeSessionMetadataJSON(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values in node metadata")
		}
		return err
	}
	return nil
}

// ACP intentionally treats malformed _meta as nil. Durable node evidence must
// not do that. Read only these dynamic leaves as raw JSON, let ACP validate the
// remaining typed question, then install our strictly decoded owner values.
func decodeSessionQuestionJSON(raw []byte, out *nodewire.SessionQuestion) error {
	var dynamic struct {
		Permission *struct {
			Options []struct {
				Meta json.RawMessage `json:"_meta"`
			}
		}
	}
	if err := json.Unmarshal(raw, &dynamic); err != nil {
		return err
	}
	var metas []acp.Meta
	if dynamic.Permission != nil {
		metas = make([]acp.Meta, len(dynamic.Permission.Options))
		for i, option := range dynamic.Permission.Options {
			if len(option.Meta) == 0 {
				continue
			}
			var meta map[string]any
			if err := decodeSessionMetadataJSON(option.Meta, &meta); err != nil {
				return fmt.Errorf("decode persisted permission metadata: %w", err)
			}
			metas[i] = acp.Meta(meta)
		}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	if out.Permission != nil {
		if len(out.Permission.Options) != len(metas) {
			return fmt.Errorf("persisted permission metadata option count differs")
		}
		for i := range out.Permission.Options {
			out.Permission.Options[i].Meta = metas[i]
		}
	}
	return nil
}

func normalizeSessionQuestions(before, next []nodewire.SessionQuestion) ([]nodewire.SessionQuestion, error) {
	previous := make(map[string]nodewire.SessionQuestion, len(before))
	for _, question := range before {
		previous[question.ID] = question
	}
	out := slices.Clone(next)
	for i, question := range out {
		if question.Permission == nil {
			continue
		}
		ask := *question.Permission
		ask.Options = slices.Clone(ask.Options)
		old := previous[question.ID].Permission
		for j := range ask.Options {
			meta := ask.Options[j].Meta
			if old != nil && j < len(old.Options) && old.Options[j].OptionID == ask.Options[j].OptionID &&
				reflect.DeepEqual(old.Options[j].Meta, meta) {
				// Equal input can still be caller-owned. Copy the trusted owner
				// value instead of retaining an external same-value map.
				ask.Options[j].Meta = copySessionJSON(old.Options[j].Meta).(acp.Meta)
				continue
			}
			var err error
			ask.Options[j].Meta, err = canonicalSessionMetadata(meta)
			if err != nil {
				return nil, fmt.Errorf("node question %s option %s metadata: %w", question.ID, ask.Options[j].OptionID, err)
			}
		}
		out[i].Permission = &ask
	}
	return out, nil
}
