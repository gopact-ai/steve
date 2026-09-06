package turn

import (
	"strings"
	"unicode"

	"github.com/gopact-ai/steve/internal/protocol"
)

// ParsedInput is the pure syntax of a prompt after the coordinator selected
// its agent. It does not authorize controls or choose any execution target.
type ParsedInput struct {
	Prompt    string
	Prefix    string
	Command   protocol.Command
	Rest      string
	Interrupt bool
}

func ParseInput(prompt string) ParsedInput {
	parsed := ParsedInput{Prompt: strings.TrimSpace(prompt)}
	for _, prefix := range []string{"!", "！", "+", "＋"} {
		if rest, ok := strings.CutPrefix(parsed.Prompt, prefix); ok && strings.TrimSpace(rest) != "" {
			parsed.Prefix = prefix
			parsed.Interrupt = prefix == "!" || prefix == "！"
			parsed.Prompt = strings.TrimSpace(rest)
			break
		}
	}
	parsed.Command, parsed.Rest = protocol.ParseCommand(parsed.Prompt)
	return parsed
}

// ImmediateInput identifies work that must reach Handle while the current
// turn is running. A syntactic @agent or /use agent prefix is skipped only
// for classification; the coordinator still resolves and authorizes it.
func ImmediateInput(input string) bool {
	_, parsed := ParseAddressedInput(input)
	return parsed.Interrupt || parsed.Control()
}

// ParseInput uses the same catalog selector as Handle without mutating the
// conversation. Transports must use it when classifying addressed controls:
// aliases and selectors without separating whitespace are valid too.
func (c *Coordinator) ParseInput(input string) (string, ParsedInput) {
	if selected, ok := c.catalog.Select(input); ok {
		return "@" + selected.Agent.ID, ParseInput(selected.Prompt)
	}
	return ParseAddressedInput(input)
}

// ParseAddressedInput preserves the selector for a transport that needs to
// insert quoted material after it. Names are still resolved only by Handle.
func ParseAddressedInput(input string) (string, ParsedInput) {
	prompt := strings.TrimSpace(input)
	address := ""
	if strings.HasPrefix(prompt, "@") {
		if i := strings.IndexFunc(prompt, unicode.IsSpace); i >= 0 {
			address = prompt[:i]
			prompt = strings.TrimSpace(prompt[i:])
		}
	} else if command, rest := protocol.ParseCommand(prompt); command == protocol.CommandUse {
		if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
			address = string(protocol.CommandUse) + " " + rest[:i]
			prompt = strings.TrimSpace(rest[i:])
		}
	}
	return address, ParseInput(prompt)
}

func (parsed ParsedInput) Control() bool {
	if parsed.Command == protocol.CommandCancel {
		return true
	}
	if parsed.Command == protocol.CommandTasks {
		verb, _, ok := parseTaskArgs(parsed.Rest)
		return ok && (verb == taskPause || verb == taskCancel)
	}
	return false
}
