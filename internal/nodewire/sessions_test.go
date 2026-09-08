package nodewire

import (
	"encoding/json"
	"testing"
)

func TestSessionActionsKeepLegacyJSON(t *testing.T) {
	for _, tt := range []struct {
		action SessionAction
		wire   string
	}{
		{SessionActionOpen, "open"},
		{SessionActionClose, "close"},
		{SessionActionPrompt, "prompt"},
		{SessionActionPoll, "poll"},
		{SessionActionAttach, "attach"},
		{SessionActionCapabilities, "capabilities"},
		{SessionActionInspectOpen, "inspect-open"},
		{SessionActionCancelOpen, "cancel-open"},
		{SessionActionSettings, "settings"},
		{SessionActionAnswer, "answer"},
		{SessionActionOption, "option"},
		{SessionActionCancel, "cancel"},
		{SessionActionAbort, "abort"},
		{SessionActionStart, "start"},
		{SessionAction("future-action"), "future-action"},
		{"", ""},
	} {
		t.Run(tt.wire, func(t *testing.T) {
			legacy := struct {
				Action string `json:"action"`
			}{tt.wire}
			raw, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			var request SessionRequest
			if err := json.Unmarshal(raw, &request); err != nil || request.Action != tt.action {
				t.Fatalf("decode legacy request: %+v, %v", request, err)
			}
			var receipt SessionOpenReceipt
			if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Action != tt.action {
				t.Fatalf("decode legacy receipt: %+v, %v", receipt, err)
			}
			for _, value := range []any{request, receipt} {
				raw, err = json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				legacy.Action = "unset"
				if err := json.Unmarshal(raw, &legacy); err != nil || legacy.Action != tt.wire {
					t.Fatalf("legacy peer decoded %q from %s: %v", legacy.Action, raw, err)
				}
			}
			raw, err = json.Marshal(SessionReply{AuthorizeAction: tt.action})
			if err != nil {
				t.Fatal(err)
			}
			var challenge struct {
				Action string `json:"authorize_action"`
			}
			if err := json.Unmarshal(raw, &challenge); err != nil || challenge.Action != tt.wire {
				t.Fatalf("legacy authorization challenge: %s, %v", raw, err)
			}
			if tt.wire == "" && string(raw) != "{}" {
				t.Fatalf("empty authorization action is no longer omitted: %s", raw)
			}
		})
	}
}

func TestSessionStatesKeepLegacyReceipts(t *testing.T) {
	for _, session := range []string{"opening", "idle", "running", "configuring", "closing", "closed", "interrupted", "future-state", ""} {
		for _, command := range []string{"accepted", "running", "completed", "cancelled", "uncertain", "future-state", ""} {
			t.Run(session+"/"+command, func(t *testing.T) {
				var state SessionState
				if err := json.Unmarshal([]byte(`{"state":"`+session+`","command":{"state":"`+command+`"}}`), &state); err != nil {
					t.Fatal(err)
				}
				if state.State.Unavailable() != (session == "closed" || session == "interrupted") {
					t.Fatalf("session availability changed: %q", session)
				}
				if state.Command.State.Active() != (command == "accepted" || command == "running") {
					t.Fatalf("command activity changed: %q", command)
				}
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				var legacy struct {
					State   string `json:"state"`
					Command struct {
						State string `json:"state"`
					} `json:"command"`
				}
				if err := json.Unmarshal(raw, &legacy); err != nil || legacy.State != session || legacy.Command.State != command {
					t.Fatalf("legacy receipt changed: %s, %v", raw, err)
				}
			})
		}
	}
}
