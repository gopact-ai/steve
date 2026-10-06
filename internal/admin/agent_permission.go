package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/permission"
)

// AgentPermission reads the target's settings, never its executable or probes.
// The policy and revision are then resolved from one coordinator snapshot.
func (a *Service) AgentPermission(ctx context.Context, node, harness string) (consoleapi.AgentPermission, error) {
	node = a.nodeKey(node)
	var readErr error
	a.ConfigStore.Read(func(c *config.Config) {
		if c == nil {
			readErr = errNoConfiguration
		}
	})
	if readErr != nil {
		return consoleapi.AgentPermission{}, readErr
	}
	var target config.Node
	if node != "" {
		var err error
		target, err = a.nodeForAgentEnrollment(textFor(ctx), node)
		if err != nil {
			return consoleapi.AgentPermission{}, err
		}
		settings, err := a.Nodes.Settings(ctx, node)
		if err != nil {
			return consoleapi.AgentPermission{}, err
		}
		declared, ok := settings.Harnesses[harness]
		if !ok || strings.TrimSpace(declared.Command) == "" {
			return consoleapi.AgentPermission{}, textFor(ctx).Errorf(i18n.AdminNodeHarnessUnregistered, node, harness)
		}
	}
	var fact consoleapi.AgentPermission
	a.ConfigStore.Read(func(c *config.Config) {
		if c == nil {
			readErr = errNoConfiguration
			return
		}
		if node != "" {
			if readErr = checkAgentNodeTarget(textFor(ctx), c, node, target); readErr != nil {
				return
			}
		}
		fact, readErr = a.agentPermission(c, node, harness)
	})
	return fact, readErr
}

// agentPermission is also used inside the Agent write callback, so a policy
// cannot change between comparison and the candidate it authorizes.
func (a *Service) agentPermission(c *config.Config, node, harness string) (consoleapi.AgentPermission, error) {
	fact := consoleapi.AgentPermission{Node: a.place(node), Harness: harness, Permission: permission.PolicyRead, Source: consoleapi.AgentPermissionDefaultRead}
	if node == "" {
		declared, ok := c.Harnesses[harness]
		if !ok || strings.TrimSpace(declared.Command) == "" && declared.Adapter == "" {
			return consoleapi.AgentPermission{}, fmt.Errorf("local harness %q is not configured", harness)
		}
	} else if _, ok := c.Nodes[node]; !ok {
		return consoleapi.AgentPermission{}, fmt.Errorf("node %q is not configured", node)
	}
	// Shared policies govern node-owned execution. A non-cluster local Host
	// uses its own harness policy; treating it as remote would invent a grant.
	if node != "" {
		if policy, ok := c.RuntimePermissions[harness]; ok {
			fact.Permission, fact.Source = policy, consoleapi.AgentPermissionSharedRemote
		}
	}
	if fact.Source != consoleapi.AgentPermissionSharedRemote {
		if policy := c.Harnesses[harness].Permission; policy != "" {
			fact.Permission, fact.Source = policy, consoleapi.AgentPermissionHubHarness
		}
	}
	if _, err := permission.New(fact.Permission); err != nil {
		return consoleapi.AgentPermission{}, fmt.Errorf("harness %q effective permission: %w", harness, err)
	}
	// Only non-secret policy and placement facts enter this revision.
	// Comparing the explicit policy remains mandatory: this is not a grant,
	// a probe result, or a launch fingerprint.
	scope := struct {
		Fact        consoleapi.AgentPermission
		Coordinator string
		Remote      bool
		TargetAddr  string
	}{Fact: fact, Coordinator: c.Gateway.HubID, Remote: node != "", TargetAddr: c.Nodes[node].Addr}
	raw, err := json.Marshal(scope)
	if err != nil {
		return consoleapi.AgentPermission{}, err
	}
	fact.Revision = fmt.Sprintf("%x", sha256.Sum256(raw))
	return fact, nil
}

func validateAgentPermissionConfirmation(req consoleapi.AddAgentRequest) error {
	if req.ExpectedPermission == nil && req.ExpectedPermissionRevision == "" {
		return nil
	}
	if req.ExpectedPermission == nil || req.ExpectedPermissionRevision == "" {
		return consoleapi.ErrAgentPermissionConfirmationInvalid
	}
	if _, err := permission.New(*req.ExpectedPermission); err != nil {
		return fmt.Errorf("%w: %v", consoleapi.ErrAgentPermissionConfirmationInvalid, err)
	}
	return nil
}

func (a *Service) confirmAgentPermission(c *config.Config, req consoleapi.AddAgentRequest) error {
	if req.ExpectedPermission == nil {
		return nil
	}
	fact, err := a.agentPermission(c, req.Node, req.Harness)
	if err != nil {
		return err
	}
	if fact.Permission != *req.ExpectedPermission || fact.Revision != req.ExpectedPermissionRevision {
		return consoleapi.ErrAgentPermissionConflict
	}
	return nil
}
