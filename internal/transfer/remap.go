package transfer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func validNumericID(id string) bool {
	if prefix, tail, ok := strings.Cut(id, "~"); ok {
		if len(prefix) != 13 || prefix[0] != 'h' {
			return false
		}
		for _, r := range prefix[1:] {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return false
			}
		}
		id = tail
	}
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func namespace(hub, id string) string {
	if id == "" {
		return ""
	}
	if strings.Contains(id, "~") {
		return id
	}
	return "h" + digest([]byte(hub))[:12] + "~" + id
}
func consoleNamespace(hub, project, id string) string {
	prefix := ""
	name := id
	if strings.HasPrefix(id, "console:") {
		prefix = "console:"
		name = strings.TrimPrefix(id, prefix)
	}
	if strings.HasPrefix(name, "h") && strings.Contains(name, "~") {
		return id
	}
	return prefix + "h" + digest([]byte(hub))[:12] + "~" + project + "~" + name
}

func operationFacts(f ledger.TransferFacts, kind string) ledger.TransferFacts {
	out := ledger.TransferFacts{}
	ids := map[string]bool{}
	for _, op := range f.Operations {
		if op.Kind == kind {
			out.Operations = append(out.Operations, op)
			ids[op.ID] = true
		}
	}
	for _, e := range f.Events {
		if ids[e.OperationID] {
			out.Events = append(out.Events, e)
		}
	}
	for _, e := range f.Effects {
		if ids[e.Effect.Operation] {
			out.Effects = append(out.Effects, e)
		}
	}
	return out
}
func remapBundle(b *Bundle, home string) error {
	m := ledger.TransferIDs{Namespace: "h" + digest([]byte(b.Owner.HubID))[:12], Tasks: map[string]string{}, Plans: map[string]string{}, Schedules: map[string]string{}, Conversations: map[string]string{}, Operations: map[string]string{}}
	for id := range b.Tasks.Tasks {
		m.Tasks[id] = namespace(b.Owner.HubID, id)
	}
	for id := range b.Plans.Plans {
		m.Plans[id] = namespace(b.Owner.HubID, id)
	}
	for id := range b.Schedules.Jobs {
		m.Schedules[id] = namespace(b.Owner.HubID, id)
	}
	for _, id := range b.Project.Conversations {
		m.Conversations[id] = consoleNamespace(b.Owner.HubID, b.Project.Project.ID, id)
	}
	for id := range b.State.Conversations {
		m.Conversations[id] = consoleNamespace(b.Owner.HubID, b.Project.Project.ID, id)
	}
	for _, id := range b.Console.Conversations {
		m.Conversations[id] = consoleNamespace(b.Owner.HubID, b.Project.Project.ID, id)
	}
	for _, j := range b.Schedules.Jobs {
		if j.ConversationID != "" {
			m.Conversations[j.ConversationID] = consoleNamespace(b.Owner.HubID, b.Project.Project.ID, j.ConversationID)
		}
	}
	for _, f := range b.Schedules.Firings {
		if f.ConversationID != "" {
			m.Conversations[f.ConversationID] = consoleNamespace(b.Owner.HubID, b.Project.Project.ID, f.ConversationID)
		}
	}
	for _, t := range b.Tasks.Tasks {
		if t.Channel != "" {
			m.Conversations[t.Channel] = consoleNamespace(b.Owner.HubID, b.Project.Project.ID, t.Channel)
		}
	}
	for _, op := range b.Facts.Operations {
		if op.Kind == "landing" && strings.HasPrefix(op.ID, "plan-sink/") {
			pieces := strings.SplitN(op.ID, "/", 4)
			if len(pieces) == 4 {
				m.Operations[op.ID] = "plan-sink/" + m.Plan(pieces[1]) + "/" + pieces[2] + "/" + pieces[3]
			}
		}
		if op.Kind == "plan-run" {
			var r exec.RunRecord
			if err := json.Unmarshal(op.Data, &r); err != nil {
				return err
			}
			m.Operations[op.ID] = "plan-run/" + m.Plan(r.PlanID)
		}
	}
	original := b.Facts
	b.Tasks.Remap(m)
	b.Plans.Remap(m)
	b.State.Remap(m)
	b.Schedules.Remap(m)
	console, err := b.Console.Remap(m.Task, m.Conversation, m.Key)
	if err != nil {
		return err
	}
	b.Console = console
	if err := b.Project.Remap(m); err != nil {
		return err
	}
	b.Project.InterruptTransferDisclosures(b.CreatedAt)
	for _, conv := range m.Conversations {
		if b.Project.Facts.Bindings["conversation-project"] == nil {
			b.Project.Facts.Bindings["conversation-project"] = map[string]json.RawMessage{}
		}
		if _, ok := b.Project.Facts.Bindings["conversation-project"][conv]; !ok {
			binding := project.Binding{ConversationID: conv, ProjectID: b.Project.Project.ID, Version: 1, By: "project-transfer", At: b.CreatedAt}
			raw, _ := json.Marshal(binding)
			b.Project.Facts.Bindings["conversation-project"][conv] = raw
			b.Project.Facts.Names = append(b.Project.Facts.Names, ledger.NamedRef{Name: "conversation/" + conv + "/project", Version: 1, Artifact: b.Project.Project.ID, UpdatedAt: b.CreatedAt})
		}
	}

	if err := b.Artifacts.Remap(m, project.Home{Path: home}); err != nil {
		return err
	}
	b.Facts = ledger.TransferFacts{}
	b.Facts.Add(b.Project.Facts)
	b.Facts.Add(b.Artifacts.Facts)
	attempts := operationFacts(original, "attempt")
	if err := attempt.RemapTransfer(&attempts, m); err != nil {
		return err
	}
	if err := exec.RemapAttemptOutputs(&attempts, m); err != nil {
		return err
	}
	b.Facts.Add(attempts)
	runs := operationFacts(original, "plan-run")
	if err := exec.RemapTransfer(&runs, m, project.Home{Path: home}); err != nil {
		return err
	}
	b.Facts.Add(runs)
	intents := operationFacts(original, "intent")
	if err := intent.RemapTransfer(&intents, m); err != nil {
		return err
	}
	b.Facts.Add(intents)
	mappingRaw, _ := json.Marshal(project.OriginMapping{Project: b.Project.Project.ID, HubID: b.Owner.HubID, TransferID: b.Owner.TransferID, IDs: m})
	b.Facts.Add(ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{"project-origin-mapping": {b.Owner.TransferID: mappingRaw}}})
	return nil
}

func validateBundle(b Bundle) error {
	if err := validateNestedGit(b.NestedGit); err != nil {
		return err
	}
	id := b.Project.Project.ID
	if id == "" || b.Owner.Project != id || b.Owner.State != "released" || b.Owner.TargetHub == "" || b.Owner.TransferID == "" {
		return fmt.Errorf("invalid ownership manifest")
	}
	taskIDs := map[string]bool{}
	for key, t := range b.Tasks.Tasks {
		if t == nil || t.ProjectID != id || t.ID != key || !validNumericID(key) {
			return fmt.Errorf("invalid task boundary")
		}
		taskIDs[key] = true
	}
	for key := range b.Plans.Plans {
		if !validNumericID(key) {
			return fmt.Errorf("invalid plan id")
		}
	}
	for key := range b.Schedules.Jobs {
		if !validNumericID(key) {
			return fmt.Errorf("invalid schedule id")
		}
	}
	artifactIDs := map[string]bool{}
	for key, raw := range b.Artifacts.Facts.Bindings["artifact"] {
		var v artifact.Manifest
		if json.Unmarshal(raw, &v) != nil || v.ID != key || v.Project != id {
			return fmt.Errorf("invalid artifact boundary")
		}
		artifactIDs[key] = true
	}
	for _, f := range []ledger.TransferFacts{b.Facts, b.Project.Facts, b.Artifacts.Facts} {
		ops := map[string]bool{}
		for _, op := range f.Operations {
			ops[op.ID] = true
			switch op.Kind {
			case "attempt":
				var r attempt.Record
				if json.Unmarshal(op.Data, &r) != nil || r.Project != id {
					return fmt.Errorf("foreign attempt")
				}
			case "plan-run":
				var r exec.RunRecord
				if json.Unmarshal(op.Data, &r) != nil || r.ProjectID != id {
					return fmt.Errorf("foreign run")
				}
			case "landing":
				var r artifact.Landing
				if json.Unmarshal(op.Data, &r) != nil || r.Project != id {
					return fmt.Errorf("foreign landing")
				}
			case "intent":
				var r intent.Intent
				if json.Unmarshal(op.Data, &r) != nil || !taskIDs[r.TaskID] {
					return fmt.Errorf("foreign effect")
				}
			case "workspace-clone":
				var r project.CloneOperation
				if json.Unmarshal(op.Data, &r) != nil || r.Project != id {
					return fmt.Errorf("foreign clone")
				}
			case "disclosure-request":
				var r project.DisclosureRequest
				if json.Unmarshal(op.Data, &r) != nil || r.Project != id {
					return fmt.Errorf("foreign disclosure")
				}
			default:
				return fmt.Errorf("unrecognized transfer operation kind %s", op.Kind)
			}
		}
		for _, e := range f.Events {
			if !ops[e.OperationID] {
				return fmt.Errorf("event references unselected operation")
			}
		}
		for _, e := range f.Effects {
			if !ops[e.Effect.Operation] {
				return fmt.Errorf("effect references unselected operation")
			}
		}
		for kind, values := range f.Bindings {
			for _, raw := range values {
				switch kind {
				case "conversation-project":
					var v project.Binding
					if json.Unmarshal(raw, &v) != nil || v.ProjectID != id {
						return fmt.Errorf("foreign conversation binding")
					}
				case "grant", "config-grant":
					var v project.Grant
					if json.Unmarshal(raw, &v) != nil || v.Project != id {
						return fmt.Errorf("foreign grant")
					}
				case "artifact", "attestation", "pending-landing", "disclosure", "project-origin-mapping", "workspace-clone":
					var v struct {
						Project string `json:"project"`
					}
					if json.Unmarshal(raw, &v) != nil || v.Project != id {
						return fmt.Errorf("foreign %s", kind)
					}
				case "replica":
					var v artifact.Replica
					if json.Unmarshal(raw, &v) != nil || !artifactIDs[v.Artifact] {
						return fmt.Errorf("foreign replica")
					}
				default:
					return fmt.Errorf("unrecognized transfer binding %s", kind)
				}
			}
		}
		for _, name := range f.Names {
			if !artifactIDs[name.Artifact] && !(strings.HasPrefix(name.Name, "conversation/") && name.Artifact == id) {
				return fmt.Errorf("foreign named reference %s", name.Name)
			}
		}
	}
	return nil
}
