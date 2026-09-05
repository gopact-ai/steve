// Mirrors internal/readmodel: the snapshot the hub serves at /state and the
// events it streams at /events. Every list is present, empty or not.
export interface Advert { node?: string; build_version?: string; hostname?: string; ips?: string[]; os?: string; arch?: string; harnesses?: Harness[]; capabilities?: string[] }
export interface Hub { node: string; started: string; capabilities?: string[]; level?: string; version?: string; advert?: Advert }
export interface Harness { id: string; command?: string; version?: string; model?: string; models?: string[]; missing?: string; slots?: number }
export interface Evidence { kind: "declared" | "observed" | "derived"; method?: string; result?: string; ok: boolean; at?: string }
export interface Capability {
    kind: string; id: string; scope?: string; version?: { scheme?: string; value: string }; availability: "available" | "unavailable" | "unknown";
    evidence?: Evidence[]; assurance?: string; attrs?: Record<string, string>; detail?: string;
}
export interface AbilitySnapshot {
    schema: string; node: string; generation: number; sequence: number; generated_at: string; received_at?: string; digest?: string;
    coverage: Record<string, string>; offers: Capability[]; features?: string[]; source?: string;
}
export interface Node {
    name: string; role?: string; version?: string; addr?: string; host?: string; ips?: string[]; up: boolean; since?: string; os?: string; arch?: string;
    capabilities?: string[]; harnesses?: Harness[]; last_error?: string; level?: string; region?: string; snapshot?: AbilitySnapshot;
    features?: string[]; health?: Health;
}
export interface Activity {
    agent: string; attempt_id?: string; kind?: string; workspace?: string; task_id?: string; step_id?: string; conversation?: string;
    tool?: string; detail?: string; since: string; at: string;
}
export interface Condition { atom: string; met: boolean; code?: string; detail?: string }
export interface Agent {
    id: string; node?: string; harness: string; model?: string; models?: string[]; eligible: boolean; why?: string;
    requires?: string[]; level?: string; slots?: number; region?: string; repair?: string; activities?: Activity[]; busy?: number; snapshot?: AbilitySnapshot;
    preferred?: string; observed?: string; conditions?: Condition[]; mcp_servers?: string[]; default?: boolean;
    options?: Record<string, string>; selectors?: Selector[]; about?: string;
}
export interface Selector { id: string; name: string; category?: string; current?: string; choices?: string[]; values?: string[] }
export interface Tokens { input?: number; output?: number; cached_read?: number; cached_write?: number; total?: number; context?: number }
export interface AttemptRow { day: string; agent: string; node?: string; model?: string; outcome?: string; started: string; seconds: number; tokens: Tokens; reported: boolean }
export interface Task {
    id: string; goal: string; state: string; lifecycle: string; execution: string; attention: number; lane: string;
    title?: string; priority?: "high" | "normal" | "low" | ""; labels?: string[]; archived_at?: string;
    member?: string; node?: string; channel?: string; project_id?: string; origin?: string; requester?: string; parent?: string; children?: string[];
    turns: number; max_turns: number; elapsed?: string; max_elapsed?: string; updated_at?: string; plan_id?: string;
    tokens?: Tokens; seconds?: number; model?: string; attempt_rows?: AttemptRow[];
}
export interface TaskMetaPatch { title?: string; priority?: Task["priority"]; labels?: string[]; archived?: boolean }
export interface StepContext { goal: string; ancestry?: string[]; refs?: string[]; findings?: string[]; facts?: string[]; bytes?: number }
export interface StepUsage { day: string; model?: string; tokens: Tokens; seconds: number }
export interface Step {
    id: string; goal: string; state: string; agent?: string; node?: string; needs?: string[]; merge?: string[];
    requires?: string[]; verify?: string; error?: string; context?: StepContext; artifact?: string; attempts?: number; usage?: StepUsage;
    started_at?: string; ended_at?: string;
}
export interface Plan { id: string; task_id: string; rev: number; goal: string; by: string; because: string; steps: Step[]; created_at?: string; base?: string; fixed?: boolean }
export interface Health { disk_free: number; disk_total: number; load1: number; worktrees: number; at: string }
export interface Admission {
    node?: string; source: "node" | "hub" | "cached" | "legacy"; verdict: number; code?: string;
    generation?: number; sequence?: number; digest?: string; atoms?: { atom: string; verdict: number; code?: string }[]; at: string;
}
export interface Attempt {
    id: string; kind: string; state: string; task_id?: string; project: string; agent?: string; node?: string;
    scope?: string; workspace?: string; leases?: string[]; started_at: string; requires?: string[]; admission?: Admission;
}
export interface Landing { id: string; project: string; state: string; artifact: string; paths?: string; error?: string; at: string }
export interface Reservation { id: string; key: string; node: string; harness: string; slots: number; for: string; by: string; expires_at: string }
export interface Attestation { artifact: string; verdict: string; by: string; note?: string; at: string }
export interface Replica { artifact: string; node: string; generation: number; state: string; note?: string; at: string }
export interface Disclosure { id: string; project: string; task_id?: string; requester: string; bytes: number; at: string }
export interface Effect { id: string; tool: string; task_id: string; attempt: string; error?: string; at: string }
export interface Grant { project: string; principal: string; role: string; by: string }
export interface Facts {
    reservations: Reservation[]; attestations: Attestation[]; replicas: Replica[]; disclosures: Disclosure[]; effects: Effect[]; grants: Grant[];
}
export interface Choice { label: string; command: string; danger?: boolean }
export interface HumanRequest {
    id: string; type: string; source: string; project_id?: string; task_id?: string; summary: string; choices: Choice[]; created_at: string; resolvable: boolean;
}
export interface Schedule { id: string; conversation: string; agent?: string; prompt: string; spec: string; next_at: string; last_at?: string; runs: number }
export interface SourceHealth { name: string; wired: boolean; error?: string }
export interface UsageRow { key: string; tokens: Tokens; seconds: number; attempts: number; unreported?: number }
export interface Usage { by_day: UsageRow[]; by_agent: UsageRow[]; by_model: UsageRow[]; total: UsageRow }
export interface Repo { path: string; branch?: string; head?: string; subject?: string; at?: string; dirty: boolean; remote?: string; agents_md: boolean; missing?: boolean }
export interface Project {
    id: string; node: string; path: string; level: string; repo: string; default_role?: string; agents: string[];
    repos?: Repo[]; home?: boolean; default?: boolean;
    // Where the project is: its home first, then its copies.
    workspaces: Workspace[];
}
export interface Workspace {
    id: string; node: string; path: string; kind: "canonical" | "copy" | "worktree" | string; origin?: string; source?: string;
    state?: "ready" | "provisioning" | "failed" | string; error?: string; busy?: boolean;
    repos?: Repo[]; agents: string[];
}
// Placement is where an agent works in a project, as the server decides.
export interface Placement { workspace: string; kind: string; node: string }
export interface ContextProject { id: string; node: string; path: string; level: string; repo: string; version: number; bound?: boolean }
export interface AgentChoice {
    id: string; node: string; harness: string; model?: string; ready: boolean; why?: string; usable: boolean; because?: string; current?: boolean;
    place?: Placement;
}
export interface ConversationContext { conversation: string; project?: ContextProject; agent?: AgentChoice; agents: AgentChoice[] }
export interface Verb { command: string; args?: string; summary: string }
export interface Suggestion { label: string; args?: string; detail?: string; insert: string; muted?: boolean }
export interface HistoryEntry { at: string; seq?: number; kind: string; subject?: string; text: string; actor?: string; operation?: string; from?: string; to?: string }
export interface ToolCall { id?: string; kind?: string; name?: string; detail?: string; status: string; input?: string; output?: string }
export interface PlanLine { text: string; status: string }
export interface Progress {
    agent?: string; node?: string; model?: string; reasoning?: string; answer?: string; tools?: ToolCall[]; plan?: PlanLine[];
}
export interface StepInfo { kind?: string; goal?: string; state?: string; since?: string; elapsed?: string; answer?: string; refs?: string[]; attempt?: string; files?: number }
export interface ChangeSummary { attempt: string; project?: string; base?: string; artifact?: string; files: number; note?: string }
export interface Change { path: string; status: string; added: number; deleted: number; binary?: boolean }
export interface ChangeIndex { attempt: string; project?: string; base?: string; artifact?: string; changes: Change[]; truncated?: boolean; note?: string }
export interface FileDiff { path: string; diff: string; truncated?: boolean }
export interface AttemptView { id: string; kind: string; state: string; agent?: string; node?: string; harness?: string; workspace?: string; base?: string; artifact?: string; summary?: string; error?: string; started_at: string; ended_at?: string; files?: number }
export interface TreeEntry { name: string; path: string; kind: "file" | "dir" | "link" | "repo"; size?: number; mode?: string }
export interface TreeView { attempt: string; commit: string; which: "result" | "base"; dir: string; entries: TreeEntry[]; truncated?: boolean }
export interface FileView { attempt: string; commit: string; path: string; text: string; size: number; binary?: boolean; truncated?: boolean }
export interface TaskDetail { task: Task; plan?: Plan; children: Task[]; attempts: AttemptView[] }
// QuoteRef points at a line of some thread to carry along with a message;
// the server reads the text, the page only keeps a preview.
export interface QuoteRef { conversation: string; reply_id: string; title?: string; excerpt?: string }
export interface Choice { Value: string; Label: string; Detail?: string }
export interface SelectorOption { ID: string; Name: string; Category?: string; Current?: string; Choices: Choice[] }
export interface Selectors { model?: string; models: Choice[]; options: SelectorOption[]; preferred?: Record<string, string> }
export interface StepProcess extends StepInfo { id: string; agent?: string; node?: string; reasoning?: string; tools?: ToolCall[] }
export interface Process { reasoning?: string; tools?: ToolCall[]; steps?: StepProcess[] }
export interface Event {
    at: string; kind: string; seq?: number; run_id?: string; task_id?: string; plan_id?: string; step_id?: string;
    state?: string; conversation?: string; text?: string; title?: string; detail?: string; progress?: Progress; step?: StepInfo; reply_id?: string;
    // n is the page's own arrival counter, so a reader can keep a cursor
    // over a buffer that is trimmed from the front.
    n?: number;
}
export interface Conversation { id: string; title: string; project?: string; agent?: string; last_at: string; count: number; running: boolean; place?: Placement; title_by?: "agent" | "user" | string; archived?: boolean }
export interface Injected {
    project?: string; workspace?: string; agent: string; node?: string; harness: string; model?: string; options?: Record<string, string>;
    session?: string; new_session: boolean; instructions_sent: boolean; instructions?: string; instructions_bytes: number; mcp_servers?: string[]; fingerprint?: string; prompt?: string;
}
export interface Reply {
    id?: string; at: string; conversation: string; input?: string; title?: string; text: string; error?: string; kind: string; process?: Process; injected?: Injected;
    changes?: ChangeSummary;
}
export interface Snapshot {
    at: string; hub: Hub; nodes: Node[]; agents: Agent[]; tasks: Task[]; plans: Plan[]; projects: Project[];
    attempts: Attempt[]; landings: Landing[]; facts: Facts; inbox: HumanRequest[]; schedules: Schedule[]; sources: SourceHealth[]; usage: Usage;
}

// Skills: what the hub can hand its agents, and what it does.
export interface SkillView { name: string; path: string; root: string; title?: string; description?: string; enabled: boolean; builtin?: boolean; source?: string; agents: string[]; projects: string[] }
export interface SkillSource { slug: string; url: string; ref?: string; subdir?: string; root: string; head?: string; fetched_at?: string; skills: string[]; error?: string }
export interface SkillNode { name: string; up: boolean; synced: boolean; takes: boolean }
export interface SkillsView { fingerprint: string; search_paths: string[]; builtin_root?: string; skills: SkillView[]; nodes: SkillNode[]; sources: SkillSource[] }
export interface SkillDoc { name: string; path: string; content: string }
export interface FoundSkill { name: string; path: string; title?: string; description?: string; loaded?: boolean }
export interface MachineSkills { name: string; hub?: boolean; up: boolean; skills: FoundSkill[]; error?: string }
// Steve's home: the three files and how much of them reaches the agent.
export interface HomeFile { name: string; text: string; bytes: number; budget: number; template?: boolean; missing?: boolean }
export interface ProjectMemory { id: string; path: string; text: string; bytes: number; budget: number; facts: number }
export interface HomeView { path: string; files: HomeFile[]; total_budget: number; owner_bytes: number; guest_bytes: number; warnings: string[]; projects: ProjectMemory[]; audit?: string }

// MCP: deployments on machines, the platform's own session servers, and
// what machines' coding agents configured themselves.
export interface MCPTool { name: string; description?: string; input_schema?: unknown }
export interface MCPProbe { at: string; ok: boolean; error?: string; stale?: boolean; tools: MCPTool[]; digest?: string; server_name?: string; server_version?: string; protocol?: string }
export interface MCPDeployment { node: string; name: string; type: string; command?: string; args?: string[]; url?: string; env_keys?: string[]; header_keys?: string[]; agents: string[]; resolvable?: boolean; provenance?: string; same_name_elsewhere?: boolean; probe?: MCPProbe }
export interface MCPPlatform { name: string; description: string; tools: { name: string; description: string }[] }
export interface MCPOwn { name: string; source: string; scope?: string; type: string; command?: string; args?: string[]; url?: string; env_keys?: string[]; header_keys?: string[]; adopted?: boolean }
export interface MCPMachine { name: string; hub?: boolean; up: boolean; unsupported?: boolean; own: MCPOwn[] }
export interface MCPView { deployments: MCPDeployment[]; platform: MCPPlatform[]; machines: MCPMachine[] }
export interface MCPRegistryEnv { name: string; description?: string; required?: boolean; secret?: boolean; default?: string }
export interface MCPRegistryPackage { registry_type: string; identifier: string; version?: string; runtime_hint?: string; transport?: string; needs?: string; env: MCPRegistryEnv[] }
export interface MCPRegistryRemote { type: string; url: string; headers: MCPRegistryEnv[] }
export interface MCPRegistryEntry { name: string; description: string; version?: string; repository?: string; packages: MCPRegistryPackage[]; remotes: MCPRegistryRemote[] }
