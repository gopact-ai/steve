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
	CommandModel   Command = "/model"
	CommandHistory Command = "/history"
	CommandTopic   Command = "/t"
	// CommandEvery and CommandAt are the scheduling verbs: standing work
	// ("every morning") and a single future moment ("at nine").
	CommandEvery     Command = "/every"
	CommandAt        Command = "/at"
	CommandSchedules Command = "/schedules"
	// CommandPlan is multi-step work: Steve decomposes it, places each step
	// on a machine that can do it, and reports the tree.
	CommandPlan  Command = "/plan"
	CommandPlans Command = "/plans"
	// CommandFleet shows which machines and agents are available right now,
	// from live adverts rather than from config.
	CommandFleet Command = "/fleet"
	// CommandProject shows or switches the conversation's project: the
	// directory work happens in is the project's, never the agent's.
	CommandProject Command = "/project"
	// CommandGrant gives a principal a role in a project; CommandApprove
	// and CommandDeny are the owner's word on a pending disclosure;
	// CommandEffects lists and resolves side effects whose outcome is
	// unknown.
	CommandGrant   Command = "/grant"
	CommandApprove Command = "/approve"
	CommandDeny    Command = "/deny"
	CommandEffects Command = "/effects"
)

func ParseCommand(input string) (Command, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return CommandUnknown, ""
	}
	switch Command(input) {
	case CommandNew, CommandClear, CommandStatus, CommandCancel, CommandSkills, CommandTasks, CommandModel, CommandHistory, CommandSchedules, CommandPlans, CommandFleet, CommandProject, CommandGrant, CommandEffects:
		return Command(input), ""
	}
	if rest, ok := prefixed(input, string(CommandPlans)); ok {
		return CommandPlans, rest
	}
	if rest, ok := prefixed(input, string(CommandPlan)); ok {
		return CommandPlan, rest
	}
	if rest, ok := prefixed(input, string(CommandFleet)); ok {
		return CommandFleet, rest
	}
	if rest, ok := prefixed(input, string(CommandProject)); ok {
		return CommandProject, rest
	}
	for _, cmd := range []Command{CommandGrant, CommandApprove, CommandDeny, CommandEffects} {
		if rest, ok := prefixed(input, string(cmd)); ok {
			return cmd, rest
		}
	}
	if rest, ok := prefixed(input, string(CommandSkills)); ok {
		return CommandSkills, rest
	}
	if rest, ok := prefixed(input, string(CommandUse)); ok {
		return CommandUse, rest
	}
	if rest, ok := prefixed(input, string(CommandModel)); ok {
		return CommandModel, rest
	}
	if rest, ok := prefixed(input, string(CommandTasks)); ok {
		return CommandTasks, rest
	}
	if rest, ok := prefixed(input, string(CommandSchedules)); ok {
		return CommandSchedules, rest
	}
	if rest, ok := prefixed(input, string(CommandEvery)); ok {
		return CommandEvery, rest
	}
	if rest, ok := prefixed(input, string(CommandAt)); ok {
		return CommandAt, rest
	}
	if rest, ok := prefixed(input, string(CommandHistory)); ok {
		return CommandHistory, rest
	}
	if rest, ok := prefixed(input, string(CommandTopic)); ok {
		return CommandTopic, rest
	}
	if rest, ok := prefixed(input, "/topic"); ok {
		return CommandTopic, rest
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
