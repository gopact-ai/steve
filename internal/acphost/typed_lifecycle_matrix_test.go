package acphost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/gopact-ai/acp"
)

type typedLifecycleParticipant struct {
	*lifecycleParticipant
	boolean bool
}

func (a *typedLifecycleParticipant) Initialize(ctx context.Context, req *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	if req.ClientCapabilities == nil || req.ClientCapabilities.Session == nil || req.ClientCapabilities.Session.ConfigOptions == nil || req.ClientCapabilities.Session.ConfigOptions.Boolean == nil {
		return nil, errors.New("client did not negotiate its implemented boolean support")
	}
	return a.lifecycleParticipant.Initialize(ctx, req)
}
func (a *typedLifecycleParticipant) option(current bool) []acp.SessionConfigOption {
	if a.boolean {
		return []acp.SessionConfigOption{acp.BooleanSessionConfigOption("opaque-option", "Opaque", current)}
	}
	value := acp.SessionConfigValueID("true")
	if !current {
		value = "false"
	}
	return []acp.SessionConfigOption{acp.SelectSessionConfigOption("opaque-option", "Opaque", value, acp.SessionConfigSelectOptions{Ungrouped: &acp.UngroupedSessionConfigSelectOptions{{Value: "false", Name: "False ID"}, {Value: "true", Name: "True ID"}}})}
}
func (a *typedLifecycleParticipant) NewSession(ctx context.Context, req *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	resp, err := a.lifecycleParticipant.NewSession(ctx, req)
	if err != nil {
		return nil, err
	}
	options := a.option(true)
	resp.ConfigOptions = &options
	return resp, nil
}
func (a *typedLifecycleParticipant) SetSessionConfigOption(_ context.Context, req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
	a.record("configure")
	if req.ConfigID != "opaque-option" {
		return nil, errors.New("option identity changed")
	}
	if a.boolean {
		value, ok := req.Value.(bool)
		if !ok || value || req.Type != acp.SetSessionConfigOptionRequestTypeBoolean {
			return nil, fmt.Errorf("not primitive boolean false: %T %v", req.Value, req.Value)
		}
	} else {
		value, ok := req.Value.(acp.SessionConfigValueID)
		if !ok || value != "false" || req.Type == acp.SetSessionConfigOptionRequestTypeBoolean {
			return nil, fmt.Errorf("not opaque select false: %T %v", req.Value, req.Value)
		}
	}
	return &acp.SetSessionConfigOptionResponse{ConfigOptions: a.option(false)}, nil
}

type typedLifecycleTransport struct{ agent *typedLifecycleParticipant }

func (typedLifecycleTransport) Name() string { return "typed-lifecycle-fixture" }
func (tr typedLifecycleTransport) Start(context.Context) (Process, error) {
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	conn, err := acp.NewAgent(input, output, func(client *acp.ClientCaller) acp.AgentHandler { tr.agent.client = client; return tr.agent })
	if err != nil {
		_ = input.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, err
	}
	p := &lifecycleProcess{conn: conn, stdin: stdin, stdout: stdout, output: output}
	tr.agent.process = p
	return p, nil
}

// Five advertised lifecycle bits crossed with two reported option wire kinds.
// The option-kind axis is not an extra Agent capability: boolean is negotiated
// by the Client, while select "false" remains an opaque value ID. This is a
// real SDK wire contract matrix, not native cleanup or stock business E2E.
func TestLifecycleWithTypedOptionsMatrix64(t *testing.T) {
	for mask := 0; mask < 32; mask++ {
		for _, boolean := range []bool{false, true} {
			t.Run(fmt.Sprintf("caps-%05b/boolean-%v", mask, boolean), func(t *testing.T) {
				cfg := SessionConfig{Workdir: t.TempDir(), MCPServers: []acp.MCPServer{acp.StdioMCPServer("fixture", "not-executed", []string{"with space", ""}, []acp.EnvVariable{{Name: "FIXTURE_VALUE", Value: "false"}})}}
				a := &typedLifecycleParticipant{lifecycleParticipant: &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(mask), workdir: cfg.Workdir, servers: cfg.MCPServers, sessions: map[acp.SessionID]bool{"stored": true}}, boolean: boolean}
				h := New(Config{Transport: typedLifecycleTransport{agent: a}, NoRestart: true})
				t.Cleanup(h.Stop)
				sid, gen, err := h.OpenSession(t.Context(), "", cfg)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"initialize", "new"}
				if err := h.SetOption(t.Context(), sid, gen, "opaque-option", "false"); err != nil {
					t.Fatal(err)
				}
				want = append(want, "configure")
				settings := h.Settings(sid)
				kind := "select"
				if boolean {
					kind = "boolean"
				}
				if len(settings.Options) != 1 || settings.Options[0].Type != kind || settings.Options[0].Current != "false" {
					t.Fatalf("wire kind/confirmed false lost: %+v", settings)
				}
				_, _, err = h.OpenSession(t.Context(), "stored", cfg)
				switch {
				case mask&2 != 0:
					want = append(want, "resume")
				case mask&1 != 0:
					want = append(want, "load")
				default:
					if !errors.Is(err, ErrResumeUnsupported) {
						t.Fatalf("restore missing cap: %v", err)
					}
				}
				if mask&3 != 0 && err != nil {
					t.Fatal(err)
				}
				cfg.ReplayHistory = true
				_, _, err = h.OpenSession(t.Context(), "history", cfg)
				if mask&1 != 0 {
					want = append(want, "load")
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrLoadUnsupported) {
					t.Fatalf("replay missing cap: %v", err)
				}
				_, err = h.ListSessions(t.Context())
				if mask&8 != 0 {
					want = append(want, "list", "list")
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrListUnsupported) {
					t.Fatalf("list missing cap: %v", err)
				}
				if err := h.CloseSession(t.Context(), sid); err != nil {
					t.Fatal(err)
				}
				if mask&4 != 0 {
					want = append(want, "close")
				}
				if knownSession(h, sid) || h.ProcessStopped(gen) {
					t.Fatal("logical close lost identity or invented native stop")
				}
				_, _, err = h.OpenSession(t.Context(), "", SessionConfig{Workdir: cfg.Workdir, MCPServers: cfg.MCPServers})
				if err != nil {
					t.Fatal(err)
				}
				want = append(want, "new")
				newSid := acp.SessionID(fmt.Sprintf("fresh-%d", a.newCount))
				err = h.DeleteSession(t.Context(), newSid)
				if mask&16 != 0 {
					want = append(want, "delete")
					if err != nil || knownSession(h, newSid) {
						t.Fatalf("delete=%v", err)
					}
				} else if !errors.Is(err, ErrDeleteUnsupported) || !knownSession(h, newSid) {
					t.Fatalf("delete missing cap: %v", err)
				}
				if got := a.methods(); !reflect.DeepEqual(got, want) {
					t.Fatalf("implicit prompt or wrong gated route: got=%v want=%v", got, want)
				}
			})
		}
	}
}
