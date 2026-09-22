package nodewire

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMCPAuthorizationRefreshWireContract(t *testing.T) {
	req := SessionRequest{MCPAuthorizationRefresh: &MCPAuthorizationRefresh{PreviousAuthorization: "Bearer revoked-test-token"}}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["mcp_authorization_refresh"]) != `{"previous_authorization":"Bearer revoked-test-token"}` {
		t.Fatalf("proof gained fields or changed shape: %s", fields["mcp_authorization_refresh"])
	}
	var decoded SessionRequest
	if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(req, decoded) {
		t.Fatalf("proof did not round trip: %v", err)
	}
	req.MCPAuthorizationRefresh = nil
	raw, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	fields = nil
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["mcp_authorization_refresh"]; exists {
		t.Fatal("nil proof changed legacy wire shape")
	}
}
