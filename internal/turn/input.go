package turn

import "github.com/gopact-ai/steve/internal/turnapi"

type ParsedInput = turnapi.ParsedInput

func ParseInput(prompt string) ParsedInput { return turnapi.ParseInput(prompt) }
func ParseAddressedInput(input string) (string, ParsedInput) {
	return turnapi.ParseAddressedInput(input)
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
