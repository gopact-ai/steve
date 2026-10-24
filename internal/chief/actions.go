package chief

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Actions are what the chief of staff asks the program to do. The model
// proposes them in its reply; the program checks and carries them out, and
// says what actually happened.
type Actions struct {
	Delegate []Delegation `json:"delegate,omitempty"`
	Stop     []string     `json:"stop,omitempty"`
}

type Delegation struct {
	Goal  string `json:"goal"`
	Agent string `json:"agent,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

const fence = "steve-actions"

var block = regexp.MustCompile("(?s)```" + fence + "\\s*\\n(.*?)```")

// parseReply splits a reply into what the owner reads and the actions. A
// reply without a block has no actions; a block that is not valid JSON, or
// more than one block, is an error and nothing in it is done.
func parseReply(reply string) (text string, actions Actions, err error) {
	found := block.FindAllStringSubmatchIndex(reply, -1)
	if len(found) == 0 {
		return strings.TrimSpace(reply), Actions{}, nil
	}
	if len(found) > 1 {
		return strings.TrimSpace(block.ReplaceAllString(reply, "")), Actions{}, fmt.Errorf("reply has %d action blocks; one is allowed", len(found))
	}
	m := found[0]
	text = strings.TrimSpace(reply[:m[0]] + reply[m[1]:])
	dec := json.NewDecoder(strings.NewReader(reply[m[2]:m[3]]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&actions); err != nil {
		return text, Actions{}, fmt.Errorf("action block: %w", err)
	}
	for i, d := range actions.Delegate {
		d.Goal = strings.TrimSpace(d.Goal)
		if d.Goal == "" {
			return text, Actions{}, fmt.Errorf("delegation %d has no goal", i+1)
		}
		if d.Mode != "" && d.Mode != "auto" && d.Mode != "yolo" {
			return text, Actions{}, fmt.Errorf("delegation %d mode %q is not auto or yolo", i+1, d.Mode)
		}
		actions.Delegate[i] = d
	}
	for i, id := range actions.Stop {
		actions.Stop[i] = strings.TrimPrefix(strings.TrimSpace(id), "#")
	}
	return text, actions, nil
}
