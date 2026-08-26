// Package protocol holds channel-neutral command and chat-type values.
package protocol

import "strings"

type ChatType string

const (
	ChatUnknown ChatType = ""
	ChatP2P     ChatType = "p2p"
	ChatGroup   ChatType = "group"
)

func ParseChatType(raw string) ChatType {
	switch ChatType(raw) {
	case ChatP2P, ChatGroup:
		return ChatType(raw)
	default:
		return ChatUnknown
	}
}

type Command string

const (
	CommandUnknown Command = ""
	CommandNew     Command = "/new"
	CommandClear   Command = "/clear"
	CommandStatus  Command = "/status"
	CommandCancel  Command = "/cancel"
	CommandSkills  Command = "/skills"
	CommandUse     Command = "/use"
	CommandTasks   Command = "/tasks"
)

func ParseCommand(input string) (Command, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return CommandUnknown, ""
	}
	switch Command(input) {
	case CommandNew, CommandClear, CommandStatus, CommandCancel, CommandSkills, CommandTasks:
		return Command(input), ""
	}
	if rest, ok := prefixed(input, string(CommandSkills)); ok {
		return CommandSkills, rest
	}
	if rest, ok := prefixed(input, string(CommandUse)); ok {
		return CommandUse, rest
	}
	return CommandUnknown, input
}

func prefixed(input, prefix string) (string, bool) {
	if input == prefix {
		return "", true
	}
	if len(input) > len(prefix) && strings.HasPrefix(input, prefix) && isSpace(input[len(prefix)]) {
		return strings.TrimSpace(input[len(prefix):]), true
	}
	return "", false
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n'
}
