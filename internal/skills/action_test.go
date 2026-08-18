package skills

import "testing"

func TestParseAction(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Action
		wantArg string
	}{
		{name: "list", input: "", want: ActionList},
		{name: "help", input: "help", want: ActionHelp},
		{name: "enable name", input: "enable remind", want: ActionEnable, wantArg: "remind"},
		{name: "enable path", input: "enable /tmp/lark-im", want: ActionEnable, wantArg: "/tmp/lark-im"},
		{name: "disable", input: "disable remind", want: ActionDisable, wantArg: "remind"},
		{name: "path add", input: "path add /tmp/catalog", want: ActionPathAdd, wantArg: "/tmp/catalog"},
		{name: "path rm", input: "path rm /tmp/catalog", want: ActionPathRemove, wantArg: "/tmp/catalog"},
		{name: "path remove", input: "path remove /tmp/catalog", want: ActionPathRemove, wantArg: "/tmp/catalog"},
		{name: "unknown", input: "explode", want: ActionUnknown},
		{name: "path missing sub", input: "path", want: ActionUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, arg := ParseAction(tt.input)
			if got != tt.want || arg != tt.wantArg {
				t.Fatalf("ParseAction(%q)=(%v,%q) want (%v,%q)", tt.input, got, arg, tt.want, tt.wantArg)
			}
		})
	}
}
