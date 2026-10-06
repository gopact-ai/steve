package acphost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

type optionWireAgent struct {
	acp.AgentHandler
	options []acp.SessionConfigOption
	client  *acp.ClientCaller
	set     func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error)
}

func (a *optionWireAgent) Initialize(_ context.Context, req *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	if req.ClientCapabilities == nil || req.ClientCapabilities.Session == nil ||
		req.ClientCapabilities.Session.ConfigOptions == nil || req.ClientCapabilities.Session.ConfigOptions.Boolean == nil {
		return nil, errors.New("boolean capability was not negotiated")
	}
	return &acp.InitializeResponse{ProtocolVersion: 1}, nil
}

func (a *optionWireAgent) NewSession(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	resp := &acp.NewSessionResponse{SessionID: "typed"}
	if a.options != nil {
		resp.ConfigOptions = &a.options
	}
	return resp, nil
}

func (a *optionWireAgent) SetSessionConfigOption(_ context.Context, req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
	return a.set(req)
}

type optionWireClient struct {
	*clientHandler
	updates chan acp.SessionUpdate
}

func (c *optionWireClient) Update(ctx context.Context, n *acp.SessionNotification) error {
	err := c.clientHandler.Update(ctx, n)
	c.updates <- n.Update
	return err
}

// Both ends use the real SDK: the handler observes the decoded wire value,
// and notifications go through the host's production client handler.
func optionWireHost(t *testing.T, initial []acp.SessionConfigOption) (*Host, *optionWireAgent, <-chan acp.SessionUpdate) {
	t.Helper()
	toAgent, fromClient := io.Pipe()
	toClient, fromAgent := io.Pipe()
	h := New(Config{})
	h.generation, h.alive = 1, true
	a := &optionWireAgent{options: initial}
	agent, err := acp.NewAgent(toAgent, fromAgent, func(c *acp.ClientCaller) acp.AgentHandler {
		a.client = c
		return a
	})
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan acp.SessionUpdate, 8)
	client, err := acp.NewClient(toClient, fromClient, func(c *acp.AgentCaller) acp.ClientHandler {
		h.caller = c
		return &optionWireClient{clientHandler: &clientHandler{h: h, generation: 1}, updates: updates}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = agent.Close()
		_ = fromAgent.Close()
		_ = fromClient.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := h.caller.Initialize(ctx, &acp.InitializeRequest{
		ProtocolVersion: 1,
		ClientCapabilities: &acp.ClientCapabilities{Session: &acp.ClientSessionCapabilities{
			ConfigOptions: &acp.SessionConfigOptionsCapabilities{Boolean: &acp.BooleanConfigOptionCapabilities{}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := h.caller.NewSession(ctx, &acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h.sessions[resp.SessionID] = newSessionState(resp.Modes, resp.ConfigOptions)
	return h, a, updates
}

func TestBooleanNotificationUpdatesModelObservation(t *testing.T) {
	h, a, updates := optionWireHost(t, wireBoolean(true))
	options := wireBoolean(false)
	model := acp.SelectSessionConfigOption("model", "Model", "corrected", acp.SessionConfigSelectOptions{
		Ungrouped: &acp.UngroupedSessionConfigSelectOptions{{Value: "corrected", Name: "Corrected Model"}},
	})
	options = append(options, model)
	if err := a.client.Update(t.Context(), &acp.SessionNotification{SessionID: "typed", Update: acp.ConfigOptionUpdateSessionUpdate(options)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("model update did not arrive")
	}
	assertOptionActual(t, h, "toggle", "boolean", "false")
	got := h.Settings("typed")
	if got.Model != "Corrected Model" || !reflect.DeepEqual(got.Models, []string{"Corrected Model"}) || got.Options[0].Category != "vendor/private" {
		t.Fatalf("updated model/opaque category observation lost: %+v", got)
	}
}

func TestBooleanRPCErrorDoesNotUndoActualNotification(t *testing.T) {
	h, a, updates := optionWireHost(t, wireBoolean(true))
	a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		if err := a.client.Update(t.Context(), &acp.SessionNotification{SessionID: "typed", Update: acp.ConfigOptionUpdateSessionUpdate(wireBoolean(false))}); err != nil {
			return nil, err
		}
		return nil, errors.New("request refused after independent update")
	}
	if err := h.SetOption(t.Context(), "typed", 1, "toggle", "true"); err == nil {
		t.Fatal("RPC failure was swallowed")
	}
	select {
	case <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("Actual notification did not arrive")
	}
	assertOptionActual(t, h, "toggle", "boolean", "false")
}

func wireBoolean(value bool) []acp.SessionConfigOption {
	category := acp.SessionConfigOptionCategory("vendor/private")
	option := acp.BooleanSessionConfigOption("toggle", "Toggle", value)
	option.Category = &category
	return []acp.SessionConfigOption{option}
}

func assertOptionActual(t *testing.T, h *Host, id, kind, current string) {
	t.Helper()
	for _, option := range h.Settings("typed").Options {
		if option.ID != id {
			continue
		}
		// Inspect the shape consumers receive across JSON as well as the
		// in-memory confirmed value.
		raw, err := json.Marshal(option)
		if err != nil {
			t.Fatal(err)
		}
		var shape map[string]any
		if err := json.Unmarshal(raw, &shape); err != nil {
			t.Fatal(err)
		}
		if shape["Type"] != kind || option.Current != current {
			t.Fatalf("Actual option = %s, want Type=%s Current=%s", raw, kind, current)
		}
		t.Logf("Actual Type=%s Current=%q valueType=%T", kind, option.Current, option.Current)
		return
	}
	t.Fatalf("option %q was dropped from Actual: %+v", id, h.Settings("typed"))
}

func TestBooleanSetOptionUsesTypedWireAndConfirmedActual(t *testing.T) {
	for _, want := range []string{"false", "true"} {
		t.Run(want, func(t *testing.T) {
			h, a, _ := optionWireHost(t, wireBoolean(want == "false"))
			a.set = func(req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
				t.Logf("wire valueType=%T value=%v discriminator=%q", req.Value, req.Value, req.Type)
				value, ok := req.Value.(bool)
				if !ok || req.Type != acp.SetSessionConfigOptionRequestTypeBoolean || value != (want == "true") {
					t.Errorf("wire valueType=%T value=%v discriminator=%q", req.Value, req.Value, req.Type)
				}
				return &acp.SetSessionConfigOptionResponse{ConfigOptions: wireBoolean(want == "true")}, nil
			}
			if err := h.SetOption(t.Context(), "typed", 1, "toggle", want); err != nil {
				t.Fatal(err)
			}
			assertOptionActual(t, h, "toggle", "boolean", want)
		})
	}
}

func TestBooleanSetOptionWaitsForAgentAndAcceptsCorrection(t *testing.T) {
	h, a, _ := optionWireHost(t, wireBoolean(false))
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	a.set = func(req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		close(entered)
		<-release
		// A successful RPC need not apply the requested true.
		return &acp.SetSessionConfigOptionResponse{ConfigOptions: wireBoolean(false)}, nil
	}
	done := make(chan error, 1)
	go func() { done <- h.SetOption(t.Context(), "typed", 1, "toggle", "true") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("set_config_option was not dispatched")
	}
	assertOptionActual(t, h, "toggle", "boolean", "false")
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertOptionActual(t, h, "toggle", "boolean", "false")
}

func TestBooleanSetOptionRejectsInvalidInputBeforeRPC(t *testing.T) {
	for _, want := range []string{"", "False", "TRUE", "0", "1", "yes", " false ", "truth"} {
		t.Run(want, func(t *testing.T) {
			h, a, _ := optionWireHost(t, wireBoolean(true))
			a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
				t.Error("invalid boolean reached the agent")
				return &acp.SetSessionConfigOptionResponse{ConfigOptions: wireBoolean(false)}, nil
			}
			if err := h.SetOption(t.Context(), "typed", 1, "toggle", want); err == nil {
				t.Fatal("invalid boolean accepted")
			}
			assertOptionActual(t, h, "toggle", "boolean", "true")
		})
	}
}

func TestBooleanSetOptionRPCErrorPreservesActual(t *testing.T) {
	h, a, _ := optionWireHost(t, wireBoolean(true))
	a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		return nil, errors.New("refused")
	}
	if err := h.SetOption(t.Context(), "typed", 1, "toggle", "false"); err == nil {
		t.Fatal("RPC rejection reported success")
	}
	assertOptionActual(t, h, "toggle", "boolean", "true")
}

func TestBooleanSetOptionKeepsSelectFalseAnOpaqueString(t *testing.T) {
	for _, value := range []string{"false", "", " opaque / 未知 "} {
		t.Run(value, func(t *testing.T) {
			initial := []acp.SessionConfigOption{acp.SelectSessionConfigOption("opaque", "Opaque", "old", acp.SessionConfigSelectOptions{})}
			h, a, _ := optionWireHost(t, initial)
			a.set = func(req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
				id, ok := req.Value.(acp.SessionConfigValueID)
				if !ok || string(id) != value || req.Type != "" {
					t.Errorf("select valueType=%T value=%v discriminator=%q", req.Value, req.Value, req.Type)
				}
				return &acp.SetSessionConfigOptionResponse{ConfigOptions: []acp.SessionConfigOption{acp.SelectSessionConfigOption("opaque", "Opaque", acp.SessionConfigValueID(value), acp.SessionConfigSelectOptions{})}}, nil
			}
			if err := h.SetOption(t.Context(), "typed", 1, "opaque", value); err != nil {
				t.Fatal(err)
			}
			assertOptionActual(t, h, "opaque", "select", value)
		})
	}
}

func TestBooleanOptionsNotificationAndFullEmptyReplacement(t *testing.T) {
	h, a, updates := optionWireHost(t, wireBoolean(true))
	progress := make(chan view.Progress, 4)
	h.collectors["typed"] = &collector{generation: 1, settings: h.sessionSettings("typed"), progress: func(p view.Progress) { progress <- p }}
	for _, options := range [][]acp.SessionConfigOption{wireBoolean(false), {}} {
		if err := a.client.Update(t.Context(), &acp.SessionNotification{SessionID: "typed", Update: acp.ConfigOptionUpdateSessionUpdate(options)}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-updates:
		case <-time.After(5 * time.Second):
			t.Fatal("notification not observed")
		}
		if len(options) > 0 {
			assertOptionActual(t, h, "toggle", "boolean", "false")
		} else if len(h.Options("typed")) != 0 || len(h.Settings("typed").Options) != 0 {
			t.Fatal("full empty update kept stale options")
		}
		select {
		case p := <-progress:
			if !reflect.DeepEqual(p.Settings, h.Settings("typed")) {
				t.Fatalf("notification/progress disagree: %+v / %+v", p.Settings, h.Settings("typed"))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("notification did not reach progress")
		}
	}
}

func TestBooleanSetOptionFullEmptyResponseClearsOptions(t *testing.T) {
	h, a, _ := optionWireHost(t, wireBoolean(true))
	a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		return &acp.SetSessionConfigOptionResponse{}, nil
	}
	if err := h.SetOption(t.Context(), "typed", 1, "toggle", "false"); err != nil {
		t.Fatal(err)
	}
	if len(h.Options("typed")) != 0 || len(h.Settings("typed").Options) != 0 {
		t.Fatal("SDK encodes the required response list as []; stale options survived")
	}
}

func TestBooleanFullEmptyNotificationClearsReportedDescriptors(t *testing.T) {
	h, a, updates := optionWireHost(t, wireBoolean(true))
	if len(h.Options("typed")) != 1 {
		t.Fatal("fixture did not start with an option")
	}
	if err := a.client.Update(t.Context(), &acp.SessionNotification{SessionID: "typed", Update: acp.ConfigOptionUpdateSessionUpdate([]acp.SessionConfigOption{})}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("full empty notification did not arrive")
	}
	if len(h.Options("typed")) != 0 {
		t.Fatal("full empty notification retained a stale descriptor")
	}
}

func TestBooleanNodeRequestStringReachesTypedACP(t *testing.T) {
	h, a, _ := optionWireHost(t, wireBoolean(true))
	a.set = func(req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		value, ok := req.Value.(bool)
		if !ok || value || req.Type != acp.SetSessionConfigOptionRequestTypeBoolean {
			t.Errorf("node false became valueType=%T value=%v type=%q", req.Value, req.Value, req.Type)
		}
		return &acp.SetSessionConfigOptionResponse{ConfigOptions: wireBoolean(false)}, nil
	}
	raw, err := json.Marshal(nodewire.SessionRequest{Action: nodewire.SessionActionOption, OptionID: "toggle", OptionValue: "false"})
	if err != nil {
		t.Fatal(err)
	}
	var request nodewire.SessionRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.OptionValue != "false" {
		t.Fatalf("node request lost false: %s", raw)
	}
	if err := h.SetOption(t.Context(), "typed", 1, acp.SessionConfigID(request.OptionID), request.OptionValue); err != nil {
		t.Fatal(err)
	}
	assertOptionActual(t, h, "toggle", "boolean", "false")
}

func TestBooleanModelAndModeRemainSelectOnly(t *testing.T) {
	for _, category := range []acp.SessionConfigOptionCategory{acp.SessionConfigOptionCategoryModel, acp.SessionConfigOptionCategoryMode} {
		t.Run(string(category), func(t *testing.T) {
			option := acp.BooleanSessionConfigOption("reserved", "Reserved", true)
			option.Category = &category
			h, a, _ := optionWireHost(t, []acp.SessionConfigOption{option})
			h.sessions["typed"].setMode("read-only")
			a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
				t.Error("boolean model/mode reached RPC")
				return &acp.SetSessionConfigOptionResponse{}, nil
			}
			if err := h.SetOption(t.Context(), "typed", 1, "reserved", "false"); err == nil {
				t.Fatal("boolean model/mode accepted")
			}
			if id, choices := h.ModelChoices("typed"); id != "" || len(choices) != 0 {
				t.Fatalf("boolean became a model selector: %q %v", id, choices)
			}
			if current, ok := h.Options("typed")[0].CurrentValue.(bool); !ok || !current {
				t.Fatal("current_mode_update rewrote a boolean as a select")
			}
		})
	}
}

func TestBooleanSetOptionNilConfirmationIsNotSuccess(t *testing.T) {
	h, a, _ := optionWireHost(t, wireBoolean(true))
	a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		return nil, nil
	}
	if err := h.SetOption(t.Context(), "typed", 1, "toggle", "false"); err == nil {
		t.Fatal("missing required response was reported as success")
	}
	assertOptionActual(t, h, "toggle", "boolean", "true")
}

func TestBooleanSetOptionDoesNotRelaxClientPermission(t *testing.T) {
	h, a, _ := optionWireHost(t, wireBoolean(false))
	a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		return &acp.SetSessionConfigOptionResponse{ConfigOptions: wireBoolean(true)}, nil
	}
	kind := acp.ToolKindExecute
	request := &acp.RequestPermissionRequest{
		SessionID: "typed",
		ToolCall:  acp.ToolCallUpdate{ToolCallID: "tool", Kind: &kind},
		Options: []acp.PermissionOption{
			{OptionID: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionID: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	}
	for _, set := range []bool{false, true} {
		if set {
			if err := h.SetOption(t.Context(), "typed", 1, "toggle", "true"); err != nil {
				t.Fatal(err)
			}
		}
		result, err := a.client.RequestPermission(t.Context(), request)
		if err != nil || result.Outcome.OptionID != "reject" {
			t.Fatalf("boolean preference relaxed permission: %+v %v", result, err)
		}
	}
}

func TestBooleanSetOptionCannotConfirmAgainstReplacedSession(t *testing.T) {
	h, a, _ := optionWireHost(t, wireBoolean(true))
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		close(entered)
		<-release
		return &acp.SetSessionConfigOptionResponse{ConfigOptions: wireBoolean(false)}, nil
	}
	done := make(chan error, 1)
	go func() { done <- h.SetOption(t.Context(), "typed", 1, "toggle", "false") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("option was not dispatched")
	}
	h.mu.Lock()
	options := wireBoolean(true)
	h.sessions["typed"] = newSessionState(nil, &options)
	h.mu.Unlock()
	release <- struct{}{}
	if err := <-done; err == nil {
		t.Fatal("replaced session accepted an old response")
	}
	assertOptionActual(t, h, "toggle", "boolean", "true")
}

func TestBooleanUnknownOrUntypedOptionNeverInventsFalse(t *testing.T) {
	h, a, _ := optionWireHost(t, wireBoolean(true))
	a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
		t.Error("unknown/untyped option reached RPC")
		return &acp.SetSessionConfigOptionResponse{}, nil
	}
	if err := h.SetOption(t.Context(), "typed", 1, "missing", "false"); err == nil {
		t.Fatal("unknown id accepted")
	}
	h.sessions["typed"].setOptions([]acp.SessionConfigOption{{ID: "untyped", Name: "Untyped", CurrentValue: nil}})
	if err := h.SetOption(t.Context(), "typed", 1, "untyped", "false"); err == nil {
		t.Fatal("missing descriptor type accepted")
	}
	state := newSessionState(nil, nil)
	if len(state.settings().Options) != 0 {
		t.Fatal("missing session metadata invented options")
	}
	options := optionsView([]acp.SessionConfigOption{{Type: acp.SessionConfigOptionTypeBoolean, ID: "unset", Name: "Unset"}})
	if len(options) != 1 || options[0].Current != "" {
		t.Fatalf("unset boolean became false: %+v", options)
	}
}

func TestBooleanNewSessionWithoutOptionsRemainsUnspecified(t *testing.T) {
	for _, options := range [][]acp.SessionConfigOption{nil, {}} {
		h, a, _ := optionWireHost(t, options)
		a.set = func(*acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
			t.Error("absent option reached the agent")
			return &acp.SetSessionConfigOptionResponse{}, nil
		}
		if len(h.Options("typed")) != 0 || len(h.Settings("typed").Options) != 0 {
			t.Fatal("nil/empty new session metadata invented settings")
		}
		if err := h.SetOption(t.Context(), "typed", 1, "toggle", "false"); err == nil {
			t.Fatal("an absent boolean was set")
		}
	}
}
