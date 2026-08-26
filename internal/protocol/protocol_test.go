package protocol

import "testing"

func TestParseChatType(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  ChatType
	}{
		{name: "p2p", input: "p2p", want: ChatP2P},
		{name: "group", input: "group", want: ChatGroup},
		{name: "unknown", input: "topic", want: ChatUnknown},
		{name: "empty", input: "", want: ChatUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseChatType(tt.input); got != tt.want {
				t.Fatalf("ParseChatType(%q)=%q want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		want     Command
		wantRest string
	}{
		{name: "new", input: "/new", want: CommandNew},
		{name: "clear", input: " /clear ", want: CommandClear},
		{name: "status", input: "/status", want: CommandStatus},
		{name: "cancel", input: "/cancel", want: CommandCancel},
		{name: "skills list", input: "/skills", want: CommandSkills},
		{name: "skills enable", input: "/skills enable remind", want: CommandSkills, wantRest: "enable remind"},
		{name: "use", input: "/use claude", want: CommandUse, wantRest: "claude"},
		{name: "not skills prefix", input: "/skillsfoo", want: CommandUnknown, wantRest: "/skillsfoo"},
		{name: "plain", input: "hello", want: CommandUnknown, wantRest: "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rest := ParseCommand(tt.input)
			if got != tt.want || rest != tt.wantRest {
				t.Fatalf("ParseCommand(%q)=(%q,%q) want (%q,%q)", tt.input, got, rest, tt.want, tt.wantRest)
			}
		})
	}
}
