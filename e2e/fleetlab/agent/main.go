// labagent is a deterministic ACP participant for the two fleet HTTP gates.
// It calls the real Steve MCP tools and works only in the supplied checkout.
// It does not access Steve stores or fabricate task/attempt/usage records.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gopact-ai/acp"
)

type agent struct {
	client   *acp.ClientCaller
	sequence atomic.Int64
	sessions sync.Map
}
type session struct {
	cwd     string
	servers []acp.MCPServer
	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
}

func (a *agent) Initialize(context.Context, *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	return &acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionV1, AgentInfo: &acp.Implementation{Name: "fleetlab-agent", Version: "1"}, AgentCapabilities: &acp.AgentCapabilities{LoadSession: true, MCPCapabilities: &acp.MCPCapabilities{HTTP: true}}}, nil
}
func (a *agent) NewSession(_ context.Context, req *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	id := acp.SessionID(fmt.Sprintf("lab-%d", a.sequence.Add(1)))
	a.sessions.Store(id, &session{cwd: req.Cwd, servers: req.MCPServers})
	return &acp.NewSessionResponse{SessionID: id}, nil
}
func (a *agent) LoadSession(_ context.Context, req *acp.LoadSessionRequest) (*acp.LoadSessionResponse, error) {
	a.sessions.Store(req.SessionID, &session{cwd: req.Cwd, servers: req.MCPServers, started: true})
	return &acp.LoadSessionResponse{}, nil
}
func (a *agent) Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
	raw, ok := a.sessions.Load(req.SessionID)
	if !ok {
		return nil, errors.New("unknown lab session")
	}
	s := raw.(*session)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.cancel = cancel
	started := s.started
	s.started = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.cancel = nil; s.mu.Unlock() }()
	var parts []string
	for _, p := range req.Prompt {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	input := strings.Join(parts, "\n")
	answer := "Recorded the delivered child result."
	var err error
	if strings.HasPrefix(strings.TrimSpace(input), "下面是一次对话的开头。") {
		answer = "Fleet acceptance"
	} else if os.Getenv("STEVE_LAB_ROLE") != "coordinator" {
		job, ok, decodeErr := parseJob(input)
		if !ok {
			return nil, errors.New("worker received no fleetlab job")
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		answer, err = work(ctx, s.cwd, job)
	} else if !started {
		switch {
		case strings.Contains(input, "Run one small fleet regression"):
			answer, err = a.delegateOne(ctx, req.SessionID, s, input)
		case strings.Contains(input, "把 kvtool 做成可发布的样子"):
			answer, err = a.autonomous(ctx, req.SessionID, s, input)
		default:
			answer = "Fleet lab ready."
		}
	}
	if err != nil {
		return nil, err
	}
	if err = a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock(answer))}); err != nil {
		return nil, err
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn, Usage: &acp.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}}, nil
}
func (a *agent) Cancel(_ context.Context, req *acp.CancelNotification) error {
	if raw, ok := a.sessions.Load(req.SessionID); ok {
		s := raw.(*session)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
	}
	return nil
}
func main() {
	conn, err := acp.NewAgent(os.Stdin, os.Stdout, func(c *acp.ClientCaller) acp.AgentHandler { return &agent{client: c} })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()
	<-conn.Done()
	if err = conn.Err(); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
