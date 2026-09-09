package agentmcp

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The platform server's own tools about the platform. A skill is text
// the agent may or may not read; a tool call arrives here, where the
// facts are live and the platform decides what comes back. steve_context
// says where the agent stands right now, steve_projects where the
// project is and what to do when it is elsewhere, steve_help the way of
// doing things by topic.

// ContextInfo is where an agent stands: as whom, on what, in which
// project and workspace, on what budget, with what attached.
type ContextInfo struct {
	Agent   string `json:"agent"`
	Channel string `json:"channel,omitempty"`
	Node    string `json:"node"`
	Harness string `json:"harness"`
	Model   string `json:"model,omitempty"`
	// Mode is "owner" in the owner's private chat, "guest" anywhere else.
	Mode        string `json:"mode"`
	Project     string `json:"project,omitempty"`
	ProjectNode string `json:"project_node,omitempty"`
	Level       string `json:"level,omitempty"`
	Repo        string `json:"repo,omitempty"`
	// Workspace is the directory this session works in and what kind it
	// is: canonical (the home), copy, or worktree; empty with a Why when
	// the agent's machine has none.
	Workspace     string   `json:"workspace,omitempty"`
	WorkspaceKind string   `json:"workspace_kind,omitempty"`
	Why           string   `json:"why,omitempty"`
	Task          string   `json:"task,omitempty"`
	Turns         int      `json:"turns,omitempty"`
	MaxTurns      int      `json:"max_turns,omitempty"`
	Elapsed       string   `json:"elapsed,omitempty"`
	MaxElapsed    string   `json:"max_elapsed,omitempty"`
	DelegatedBy   string   `json:"delegated_by,omitempty"`
	MCPServers    []string `json:"mcp_servers"`
	Skills        []string `json:"skills"`
	// Notes are what the platform does for the agent, so it need not.
	Notes []string `json:"notes"`
}

// Informer answers the platform's questions about a session.
type Informer interface {
	Context(ctx context.Context, conversationID, agentID string) (ContextInfo, error)
	// Projects lists every project, where each is, and which this agent
	// can work in from its machine, with the ways out for the others.
	Projects(ctx context.Context, conversationID, agentID string) (string, error)
}

// SetInformer wires the platform's questions.
func (s *Server) SetInformer(i Informer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.informer = i
}

//go:embed help/*.md
var helpFS embed.FS

// helpTopics is what steve_help offers, in the order a reader wants.
var helpTopics = []struct{ id, about string }{
	{"overview", "Steve 是什么、你在哪、开工时拿到了什么、平台替你做的事"},
	{"delegate", "怎么看有谁、把目标写得让别人能动手、传引用、等结果、什么时候不委派"},
	{"projects", "项目 / 主目录 / 副本 / 工作树，改动怎么被记录和落地，被告知项目在别的机器时怎么办"},
	{"plans", "任务与回合预算、计划步骤怎么跑和验证、在步骤里该怎么做、定时与修复"},
	{"memory", "三份档案何时注入、群聊与访客、什么值得记、怎么记"},
	{"channel", "什么时候一张进度卡有用、怎么更新同一张、最终答案怎么到用户那里"},
}

func helpText(topic string) (string, error) {
	topic = strings.ToLower(strings.TrimSpace(topic))
	if topic == "" {
		var b strings.Builder
		b.WriteString("Topics — call steve_help with one:\n")
		for _, t := range helpTopics {
			fmt.Fprintf(&b, "- %s: %s\n", t.id, t.about)
		}
		return b.String(), nil
	}
	for _, t := range helpTopics {
		if t.id == topic {
			raw, err := helpFS.ReadFile("help/" + topic + ".md")
			if err != nil {
				return "", err
			}
			return string(raw), nil
		}
	}
	ids := make([]string, 0, len(helpTopics))
	for _, t := range helpTopics {
		ids = append(ids, t.id)
	}
	return "", fmt.Errorf("no topic %q; topics are %s", topic, strings.Join(ids, ", "))
}

func (s *Server) steveContext(ctx context.Context, bind binding) (string, error) {
	s.mu.Lock()
	if err := s.loadConversationLocked(ctx, bind.conversationID, nil); err != nil {
		s.mu.Unlock()
		return "", err
	}
	informer := s.informer
	boundChannel := ""
	if a := s.anchors[bind.conversationID]; a != nil {
		boundChannel = a.address.Channel
	}
	s.mu.Unlock()
	if informer == nil {
		return "", errors.New("steve_context is not wired on this gateway")
	}
	info, err := informer.Context(ctx, bind.conversationID, bind.agentID)
	if err != nil {
		return "", err
	}
	info.DelegatedBy = bind.delegatedBy
	info.Channel = boundChannel
	// Anyone but the owner in private gets the shape without the
	// places: which project, what kind of workspace, but no paths.
	if info.Mode != "owner" {
		info.Workspace = ""
		info.Notes = append(info.Notes, "这不是用户的私聊：用户的档案没有注入，目录路径不显示。")
	}
	if info.MCPServers == nil {
		info.MCPServers = []string{}
	}
	if info.Skills == nil {
		info.Skills = []string{}
	}
	info.Notes = append(info.Notes,
		"你的最终回答由平台投递给用户，不要用工具重复发。",
		"同一个目录同一时刻只有一个写者；租约、快照、落地由 hub 管。",
		"写文件的范围由工具权限控制；工作区之外的路径会被拒或要求确认。",
		"做法按主题看 steve_help：overview, delegate, projects, plans, memory, channel。")
	sort.Strings(info.MCPServers)
	sort.Strings(info.Skills)
	raw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Server) steveProjects(ctx context.Context, bind binding) (string, error) {
	s.mu.Lock()
	informer := s.informer
	s.mu.Unlock()
	if informer == nil {
		return "", errors.New("steve_projects is not wired on this gateway")
	}
	return informer.Projects(ctx, bind.conversationID, bind.agentID)
}

func (s *Server) steveHelp(raw json.RawMessage) (string, error) {
	var args struct {
		Topic string `json:"topic"`
	}
	if len(raw) > 0 {
		// Unreadable arguments read as no topic: help answers with the
		// overview rather than refusing to help.
		_ = json.Unmarshal(raw, &args)
	}
	return helpText(args.Topic)
}

// informTools are the schemas of the three, offered when an informer is wired.
func informTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "steve_context",
			"description": "Where you stand right now in Steve: which agent you are and on which machine, the project and the workspace directory you are in (the home, a copy, or a worktree — and why none, if none), your task's remaining budget, whether this is the owner's private chat or a group, and which MCP servers and skills you have. Read this when an answer or action needs live state not established by the supplied context, or when operational details may be stale. Greetings and ordinary questions need no context call. It changes nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "steve_projects",
			"description": "Every project Steve knows, where each one is (its home machine and copies), and which of them you can work in from your machine. When the conversation's project is not on your machine it says the ways out: an agent on the right machine, a copy on yours, or another project. It changes nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "steve_help",
			"description": "How things are done in Steve, by topic: overview, delegate, projects, plans, memory, channel. Without a topic it lists them. Read the topic before delegating, before working in a plan step, before remembering something for the user, before sending a progress card.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"topic": map[string]any{"type": "string", "description": "One of: overview, delegate, projects, plans, memory, channel."},
			}},
		},
	}
}

// Fleeter maintains the fleet for the platform's own tools: what the
// machines are and how they are, and adding or forgetting one. The
// owner in private may change the fleet; anyone else may only look.
type Fleeter interface {
	Nodes(ctx context.Context) (string, error)
	AddNode(ctx context.Context, name, addr, level, hubURL string) (string, error)
	RemoveNode(ctx context.Context, name string) error
	RefreshNode(ctx context.Context, name string) (string, error)
}

// SetFleeter wires fleet maintenance.
func (s *Server) SetFleeter(f Fleeter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fleeter = f
}

func (s *Server) fleeterFor(bind binding, change bool) (Fleeter, error) {
	s.mu.Lock()
	f, informer := s.fleeter, s.informer
	s.mu.Unlock()
	if f == nil {
		return nil, errors.New("fleet maintenance is not wired on this gateway")
	}
	if change {
		if informer == nil {
			return nil, errors.New("cannot tell who is asking; the fleet stays as it is")
		}
		info, err := informer.Context(context.Background(), bind.conversationID, bind.agentID)
		if err != nil {
			return nil, err
		}
		if info.Mode != "owner" || bind.delegatedBy != "" {
			return nil, errors.New("only the owner, in private, may change the fleet; ask them")
		}
	}
	return f, nil
}

func (s *Server) steveNodes(ctx context.Context, bind binding) (string, error) {
	f, err := s.fleeterFor(bind, false)
	if err != nil {
		return "", err
	}
	return f.Nodes(ctx)
}

func (s *Server) steveNodeAdd(ctx context.Context, bind binding, raw json.RawMessage) (string, error) {
	f, err := s.fleeterFor(bind, true)
	if err != nil {
		return "", err
	}
	var args struct {
		Name   string `json:"name"`
		Addr   string `json:"addr"`
		Level  string `json:"level"`
		HubURL string `json:"hub_url"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", errors.New("bad steve_node_add arguments")
	}
	return f.AddNode(ctx, args.Name, args.Addr, args.Level, args.HubURL)
}

func (s *Server) steveNodeRemove(ctx context.Context, bind binding, raw json.RawMessage) (string, error) {
	f, err := s.fleeterFor(bind, true)
	if err != nil {
		return "", err
	}
	var args struct{ Name string }
	if err := json.Unmarshal(raw, &args); err != nil || args.Name == "" {
		return "", errors.New("steve_node_remove needs a name")
	}
	if err := f.RemoveNode(ctx, args.Name); err != nil {
		return "", err
	}
	return "机器 " + args.Name + " 已从 hub 忘掉：不再拨号、不再列出；它上面的进程没有动。", nil
}

func (s *Server) steveNodeRefresh(ctx context.Context, bind binding, raw json.RawMessage) (string, error) {
	f, err := s.fleeterFor(bind, false)
	if err != nil {
		return "", err
	}
	var args struct{ Name string }
	if err := json.Unmarshal(raw, &args); err != nil || args.Name == "" {
		return "", errors.New("steve_node_refresh needs a name")
	}
	return f.RefreshNode(ctx, args.Name)
}

// fleetTools are the schemas of the four, offered when a fleeter is wired.
func fleetTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "steve_nodes",
			"description": "Every machine in the fleet and how it is right now: up or down and since when, version, data level, health (free disk, load, worktrees), the AI tools on it and whether each can start, MCP servers, skills of its own, and the agents placed on it. Read this before placing work, delegating by capability, or judging whether a machine is in trouble. steve_fleet lists agents; this lists machines. It changes nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "steve_node_add",
			"description": "Add a machine to the fleet: the hub records it with a fresh token and returns the one-line bootstrap command to run on that machine, which installs steve-node and connects it. Owner-only, in private. Confirm the name, address and data level with the owner first.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"name":    map[string]any{"type": "string", "description": "Machine name: lowercase letters, digits, dot, underscore, dash, e.g. node-c."},
				"addr":    map[string]any{"type": "string", "description": "Where the hub dials it: host:port, e.g. 10.0.0.5:7701."},
				"level":   map[string]any{"type": "string", "description": "Data level the hub assigns: public, internal, restricted or sealed. Default internal."},
				"hub_url": map[string]any{"type": "string", "description": "How that machine reaches the hub's console, e.g. http://10.0.0.1:7710; needed for the bootstrap command. Optional when the hub already knows one."},
			}, "required": []string{"name", "addr"}},
		},
		{
			"name":        "steve_node_remove",
			"description": "Forget a machine: the hub stops dialing and listing it. Refused while an agent is placed on it or a project has its home or a copy there — move those first. Owner-only, in private. Nothing on the machine itself is touched.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"name": map[string]any{"type": "string"},
			}, "required": []string{"name"}},
		},
		{
			"name":        "steve_node_refresh",
			"description": "Ask a machine to look at itself again now — its AI tools, health, skills, MCP servers — and return what it says. Use it when a machine was just repaired or seems stale. It changes nothing on the machine.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"name": map[string]any{"type": "string"},
			}, "required": []string{"name"}},
		},
	}
}
