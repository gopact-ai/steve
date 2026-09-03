import { useCallback, useEffect, useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkBreaks from "remark-breaks";
import { CheckCircle, ChevronDown, Loading01, MessageChatSquare, Send01, XCircle } from "@untitledui/icons";
import { Avatar } from "@/components/base/avatar/avatar";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { fetchContext, fetchReplies, fetchVerbs, send, when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { ConversationContext, Event, Plan, Process, Progress, Project, Reply, Snapshot, Step, StepProcess, ToolCall, Verb } from "@/lib/types";
import { Mono, Nothing, StateBadge } from "@/lib/ui";


// Live is what the current line is doing: the turn's own progress, and
// each plan step's, until the reply lands.
interface Live { since: string; turn?: Progress; steps: Record<string, Progress>; order: string[] }

export function ConsolePage() {
    const { snap, consoleEvents, refresh } = useFleet();
    const { intent } = useIntent();
    const [conversation, setConversation] = useState("console:main");
    const [entries, setEntries] = useState<Reply[]>([]);
    const [known, setKnown] = useState<string[]>([]);
    const [enabled, setEnabled] = useState(true);
    const [text, setText] = useState("");
    const [busy, setBusy] = useState(false);
    const [status, setStatus] = useState("");
    const [live, setLive] = useState<Live | null>(null);
    const [context, setContext] = useState<ConversationContext | null>(null);
    const [verbs, setVerbs] = useState<Verb[]>([]);
    const [pick, setPick] = useState(0);
    const box = useRef<HTMLTextAreaElement>(null);
    const bottom = useRef<HTMLDivElement>(null);
    const seen = useRef(0);
    const handled = useRef(0);

    const loadContext = useCallback(() => {
        void fetchContext(conversation).then((data) => setContext(data.context ?? null)).catch(() => undefined);
    }, [conversation]);

    useEffect(() => {
        void (async () => {
            try {
                const data = await fetchReplies(conversation);
                setEnabled(data.enabled);
                setEntries(data.replies || []);
                setKnown(data.conversations || []);
            } catch (e) { setStatus(String(e)); }
        })();
        loadContext();
    }, [conversation, loadContext]);

    useEffect(() => { void fetchVerbs().then((data) => setVerbs(data.verbs || [])).catch(() => undefined); }, []);
    // The context depends on the fleet: an agent coming up changes who can work here.
    useEffect(() => { loadContext(); }, [snap.at, loadContext]);

    useEffect(() => {
        const fresh = consoleEvents.slice(seen.current);
        seen.current = consoleEvents.length;
        const mine = fresh.filter((ev) => ev.conversation === conversation);
        if (!mine.length) return;
        setLive((cur) => mine.reduce(applyLive, cur));
        const lines = mine.filter((ev) => ev.kind.startsWith("console.") && ev.kind !== "console.progress");
        if (!lines.length) return;
        setEntries((list) => {
            const next = [...list];
            for (const ev of lines) {
                const kind = ev.kind.slice("console.".length);
                const r: Reply = { at: ev.at, conversation, kind, title: ev.title, text: kind === "sent" ? "" : ev.text || "", input: kind === "sent" ? ev.text : undefined };
                if (!next.some((x) => x.at === r.at && x.kind === r.kind && (x.text === r.text || x.input === r.input))) next.push(r);
            }
            return next.slice(-200);
        });
    }, [consoleEvents, conversation]);

    // A reply carries its process; the stream's copy of the reply does
    // not, so pull the stored one once the line has landed.
    useEffect(() => {
        if (live || !entries.length) return;
        const last = entries[entries.length - 1];
        if (last.kind !== "reply" || last.process) return;
        void fetchReplies(conversation).then((data) => setEntries(data.replies || [])).catch(() => undefined);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [live]);

    useEffect(() => { bottom.current?.scrollIntoView({ block: "end" }); }, [entries, live]);

    useEffect(() => {
        if (!intent || intent.n === handled.current) return;
        handled.current = intent.n;
        if (intent.mode === "fill") { setText(intent.text + " "); box.current?.focus(); }
        else void submit(intent.text);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [intent]);

    async function submit(line?: string) {
        const input = (line ?? text).trim();
        if (!input || busy) return;
        setText("");
        setBusy(true);
        setStatus("running…");
        try {
            const reply = await send(conversation, input);
            setStatus("");
            if (reply?.process) setEntries((list) => list.map((x) => (x.at === reply.at && x.kind === "reply" ? reply : x)));
        } catch (e) {
            setStatus(String(e).replace(/^Error: /, ""));
        } finally {
            setBusy(false);
            setLive(null);
            refresh();
            loadContext();
            box.current?.focus();
        }
    }

    const conversations = Array.from(new Set(["console:main", ...known, ...snap.tasks.map((t) => t.channel || "").filter((c) => c.startsWith("console:"))])).sort();
    const items = conversations.map((c) => ({ id: c, label: c }));
    const settled = ["done", "failed", "skipped", "cancelled"];
    const runningPlans = snap.plans.filter((p) => {
        const task = snap.tasks.find((t) => t.id === p.task_id);
        return task && task.channel === conversation && (p.steps || []).some((s) => !settled.includes(s.state));
    });

    // Suggestions: what the line so far could be completed with, from the
    // verbs the coordinator offers and the fleet the snapshot shows.
    const suggestions = suggest(text, verbs, context, snap);
    useEffect(() => { setPick(0); }, [text]);
    function apply(item: Suggestion) {
        setText(item.insert);
        box.current?.focus();
    }
    function onKey(e: React.KeyboardEvent<HTMLTextAreaElement>) {
        if (suggestions.length) {
            if (e.key === "ArrowDown") { e.preventDefault(); setPick((i) => (i + 1) % suggestions.length); return; }
            if (e.key === "ArrowUp") { e.preventDefault(); setPick((i) => (i - 1 + suggestions.length) % suggestions.length); return; }
            if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey && suggestions[pick] && suggestions[pick].insert !== text)) {
                e.preventDefault(); apply(suggestions[pick]); return;
            }
            if (e.key === "Escape") { e.preventDefault(); setText(text + " "); return; }
        }
        if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); void submit(); }
    }

    return (
        <div className="flex h-full flex-col">
            <header className="flex items-center gap-3 border-b border-secondary bg-primary px-6 py-3">
                <div className="w-56">
                    <Select aria-label="Conversation" size="sm" selectedKey={conversation} onSelectionChange={(k) => k && setConversation(String(k))} items={items}>
                        {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                    </Select>
                </div>
                <span className="text-xs text-tertiary">Talk to the current agent, or type <span className="font-mono">/</span> for what Steve can do and <span className="font-mono">@</span> for who.</span>
                <span className="ml-auto text-xs text-tertiary">{status}</span>
            </header>

            <div className="min-h-0 flex-1 overflow-y-auto overflow-x-hidden px-6 py-5">
                {!enabled && <Nothing icon={MessageChatSquare} title="The console is off">Set feishu.owner_open_id — the console acts as the owner.</Nothing>}
                {enabled && entries.length === 0 && !live && (
                    <Nothing icon={MessageChatSquare} title="Nothing said here yet">Say what you want done. Type / to see the verbs, @ to pick an agent.</Nothing>
                )}
                <div className="mx-auto flex max-w-4xl flex-col gap-5">
                    {entries.map((r, i) => <Message key={i} r={r} />)}
                    {live && <Working live={live} plans={runningPlans} />}
                    <div ref={bottom} />
                </div>
            </div>

            <div className="border-t border-secondary bg-primary px-6 py-3">
                <div className="mx-auto flex max-w-4xl flex-col gap-2">
                    <ContextBar context={context} projects={snap.projects} onProject={(id) => void submit(`/project use ${id}`)} onAgent={(id) => void submit(`/use ${id}`)} />
                    <div className="relative flex items-end gap-3">
                        {suggestions.length > 0 && (
                            <div className="absolute bottom-full left-0 z-10 mb-2 w-full max-w-2xl overflow-hidden rounded-xl bg-primary shadow-lg ring-1 ring-secondary">
                                <ul className="max-h-72 overflow-y-auto py-1">
                                    {suggestions.map((sg, i) => (
                                        <li key={sg.insert + i}>
                                            <button type="button" onMouseDown={(e) => { e.preventDefault(); apply(sg); }}
                                                className={`flex w-full items-baseline gap-3 px-3 py-1.5 text-left text-sm ${i === pick ? "bg-secondary" : "hover:bg-secondary"} ${sg.muted ? "opacity-60" : ""}`}>
                                                <span className="shrink-0 font-mono text-xs text-primary">{sg.label}</span>
                                                {sg.args && <span className="shrink-0 font-mono text-xs text-quaternary">{sg.args}</span>}
                                                <span className="truncate text-xs text-tertiary">{sg.detail}</span>
                                            </button>
                                        </li>
                                    ))}
                                </ul>
                                <div className="border-t border-secondary px-3 py-1 text-[11px] text-quaternary">↑↓ choose · Tab fill · Enter send · Esc dismiss</div>
                            </div>
                        )}
                        <TextArea
                            aria-label="Message"
                            textAreaRef={box}
                            value={text}
                            rows={2}
                            placeholder="Say what you want done. / for verbs, @ for agents. Enter sends, Shift+Enter breaks a line."
                            onChange={(v) => setText(v)}
                            onKeyDown={onKey}
                            className="flex-1"
                        />
                        <Button size="lg" color="primary" iconTrailing={Send01} isLoading={busy} isDisabled={!text.trim()} onClick={() => void submit()}>
                            Send
                        </Button>
                    </div>
                </div>
            </div>
        </div>
    );
}

// ContextBar is where this conversation stands: the project (and so the
// machine and directory), the current agent, and who else could take the
// next line — by the same rules a turn is judged by.
function ContextBar({ context, projects, onProject, onAgent }: { context: ConversationContext | null; projects: Project[]; onProject: (id: string) => void; onAgent: (id: string) => void }) {
    if (!context) return null;
    const usable = context.agents.filter((a) => a.usable);
    const elsewhere = context.agents.filter((a) => !a.usable);
    const projectItems = projects.map((p) => ({ id: p.id, label: p.id, supportingText: `${p.node} · ${p.path}` }));
    const agentItems = context.agents.map((a) => ({ id: a.id, label: a.id, supportingText: `${a.node} · ${a.harness}${a.model ? " · " + a.model : ""}${a.usable ? "" : " · " + (a.because || a.why || "")}`, isDisabled: !a.usable }));
    return (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs text-tertiary">
            <div className="flex items-center gap-2">
                <span className="text-quaternary" title="The project decides the machine and the directory the work happens in.">Project</span>
                <div className="w-64">
                    <Select aria-label="Project" size="sm" selectedKey={context.project?.id ?? null} onSelectionChange={(k) => k && String(k) !== context.project?.id && onProject(String(k))} items={projectItems}>
                        {(item) => <Select.Item id={item.id} supportingText={item.supportingText}>{item.label}</Select.Item>}
                    </Select>
                </div>
                {context.project && (
                    <span className="flex items-center gap-1.5">
                        <Mono>{context.project.node}</Mono>
                        <Mono className="text-quaternary">{context.project.path}</Mono>
                        <Badge type="modern" size="sm" color="gray">{context.project.level}</Badge>
                        <Badge type="modern" size="sm" color="gray">{context.project.repo}</Badge>
                    </span>
                )}
            </div>
            <div className="flex items-center gap-2">
                <span className="text-quaternary" title="Who answers a plain line. @ another agent to switch.">Agent</span>
                <div className="w-64">
                    <Select aria-label="Agent" size="sm" selectedKey={context.agent?.id ?? null} onSelectionChange={(k) => k && String(k) !== context.agent?.id && onAgent(String(k))} items={agentItems}>
                        {(item) => <Select.Item id={item.id} supportingText={item.supportingText} isDisabled={item.isDisabled}>{item.label}</Select.Item>}
                    </Select>
                </div>
                {context.agent && <span>{context.agent.node} · {context.agent.harness}{context.agent.model ? ` · ${context.agent.model}` : ""} · {context.agent.ready ? <span className="text-success-primary">ready</span> : <span className="text-error-primary">blocked</span>}</span>}
            </div>
            <div className="flex items-center gap-1.5">
                <span className="text-quaternary">Can work here</span>
                {usable.length ? usable.map((a) => <Mono key={a.id}>{a.id}</Mono>) : <span className="text-error-primary">nobody — pick a project homed where an agent is</span>}
                {elsewhere.length > 0 && (
                    <span className="ml-2 text-quaternary" title={elsewhere.map((a) => `${a.id}: ${a.because || a.why}`).join("\n")}>
                        not here: {elsewhere.map((a) => a.id).join(", ")}
                    </span>
                )}
            </div>
        </div>
    );
}

interface Suggestion { label: string; args?: string; detail: string; insert: string; muted?: boolean }

// suggest is the completion for the line so far. Every candidate comes from
// the coordinator (verbs), the context (agents) or the snapshot (projects,
// tasks, pending decisions); the page keeps no list of its own.
function suggest(text: string, verbs: Verb[], context: ConversationContext | null, snap: Snapshot): Suggestion[] {
    if (!text || text.includes("\n")) return [];
    const agents = context?.agents ?? [];
    const at = text.match(/(^|\s)@([\w-]*)$/);
    if (at) {
        const head = text.slice(0, text.length - at[2].length - 1);
        return agents.filter((a) => a.id.startsWith(at[2])).map((a) => ({
            label: "@" + a.id, detail: `${a.node} · ${a.harness}${a.model ? " · " + a.model : ""} · ${a.usable ? "ready here" : a.because || a.why || "not here"}`,
            insert: `${head}@${a.id} `, muted: !a.usable,
        }));
    }
    if (!text.startsWith("/")) return [];
    const m = text.match(/^(\/[a-z]*)(\s+(.*))?$/);
    if (!m) return [];
    const verb = m[1];
    const rest = m[3] ?? "";
    const hasSpace = m[2] !== undefined;
    if (!hasSpace) {
        return verbs.filter((v) => v.command.startsWith(verb)).map((v) => ({ label: v.command, args: v.args, detail: v.summary, insert: v.args ? v.command + " " : v.command }));
    }
    const pick = (list: Suggestion[]) => list.filter((s) => s.insert.startsWith(text) || s.label.toLowerCase().includes(rest.toLowerCase().split(" ").pop() || ""));
    switch (verb) {
        case "/project": {
            const sub = rest.startsWith("use ") ? rest.slice(4) : rest;
            if (!rest.startsWith("use")) return [{ label: "/project use", args: "<id>", detail: "switch this conversation's project", insert: "/project use " }];
            return snap.projects.filter((p) => p.id.startsWith(sub)).map((p) => ({ label: p.id, detail: `${p.node} · ${p.path} · ${p.level} · ${p.repo} · agents: ${p.agents.join(" ") || "none"}`, insert: `/project use ${p.id}` }));
        }
        case "/use":
        case "/repair":
            return agents.filter((a) => a.id.startsWith(rest) && (verb === "/use" || !a.ready)).map((a) => ({ label: a.id, detail: `${a.node} · ${a.harness} · ${a.usable ? "ready here" : a.because || a.why || ""}`, insert: `${verb} ${a.id}`, muted: verb === "/use" && !a.usable }));
        case "/tasks": {
            const sub = rest.replace(/^(pause|resume|cancel)\s+/, "");
            const prefix = rest.match(/^(pause|resume|cancel)\s+/)?.[1];
            const ops: Suggestion[] = prefix ? [] : ["pause", "resume", "cancel"].filter((o) => o.startsWith(rest)).map((o) => ({ label: o, args: "<id>", detail: `${o} a task`, insert: `/tasks ${o} ` }));
            const tasks = snap.tasks.filter((t) => t.id.startsWith(sub)).slice(0, 12).map((t) => ({ label: "#" + t.id, detail: `${t.state} · ${t.member || ""} · ${t.goal}`, insert: `/tasks ${prefix ? prefix + " " : ""}${t.id}` }));
            return [...ops, ...tasks];
        }
        case "/plans":
            return snap.plans.filter((p) => p.id.startsWith(rest)).slice(0, 12).map((p) => ({ label: "plan " + p.id, detail: `rev ${p.rev} · ${p.by} · ${p.goal}`, insert: `/plans ${p.id}` }));
        case "/approve":
        case "/deny":
            return pick(snap.facts.disclosures.map((d) => ({ label: d.id, detail: `${d.project} · ${d.requester || ""} · ${d.bytes} bytes`, insert: `${verb} ${d.id}` })));
        case "/effects":
            return pick(snap.facts.effects.map((e) => ({ label: e.id, detail: `${e.tool} · task #${e.task_id}${e.error ? " · " + e.error : ""}`, insert: `/effects ${e.id} ` })));
        default:
            return [];
    }
}

// applyLive folds one event into the live view: a sent line opens it, a
// reply closes it, progress fills it in.
function applyLive(cur: Live | null, ev: Event): Live | null {
    switch (ev.kind) {
        case "console.sent":
            return { since: ev.at, steps: {}, order: [] };
        case "console.reply":
        case "console.notice":
            return null;
        case "console.progress":
            return { ...(cur ?? { since: ev.at, steps: {}, order: [] }), turn: ev.progress };
        case "step.progress": {
            const base = cur ?? { since: ev.at, steps: {}, order: [] };
            const id = ev.step_id || "?";
            return { ...base, steps: { ...base.steps, [id]: ev.progress || {} }, order: base.order.includes(id) ? base.order : [...base.order, id] };
        }
        default:
            return cur;
    }
}

function Message({ r }: { r: Reply }) {
    if (r.kind === "sent") {
        return (
            <div className="flex justify-end">
                <div className="flex max-w-[80%] flex-col items-end gap-1">
                    <span className="text-xs text-quaternary">you · {when(r.at)}</span>
                    <div className="rounded-2xl rounded-tr-sm bg-brand-solid px-4 py-2.5 text-sm text-white shadow-xs whitespace-pre-wrap break-words [overflow-wrap:anywhere]">{r.input}</div>
                </div>
            </div>
        );
    }
    const tone = r.error ? "error" : r.kind === "milestone" ? "success" : r.kind === "notice" ? "warning" : "gray";
    return (
        <div className="flex min-w-0 gap-3">
            <Avatar size="sm" initials="S" alt="steve" className="mt-5 shrink-0" />
            <div className="flex min-w-0 max-w-[85%] flex-col gap-1">
                <div className="flex items-center gap-2 text-xs text-quaternary">
                    <span>steve · {when(r.at)}</span>
                    {r.kind !== "reply" && <Badge type="pill-color" size="sm" color={tone}>{r.kind}</Badge>}
                    {r.error && <Badge type="pill-color" size="sm" color="error">error</Badge>}
                </div>
                <div className={`min-w-0 rounded-2xl rounded-tl-sm bg-primary px-4 py-3 shadow-xs ring-1 ring-secondary ring-inset ${r.error ? "ring-error" : ""}`}>
                    {r.title && <div className="mb-1 text-sm font-semibold text-primary">{r.title}</div>}
                    <div className="md prose prose-sm max-w-none break-words [overflow-wrap:anywhere]">
                        <Markdown remarkPlugins={[remarkBreaks]}>{r.text}</Markdown>
                    </div>
                    {r.process && <ProcessFold process={r.process} />}
                </div>
            </div>
        </div>
    );
}

// Working is the bubble for the line in flight: plan steps with their
// states, and under each the agent's reasoning and tool calls as they
// happen; or, for a plain turn, the agent's own.
function Working({ live, plans }: { live: Live; plans: Plan[] }) {
    const [now, setNow] = useState(Date.now());
    useEffect(() => { const t = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(t); }, []);
    const elapsed = Math.max(0, Math.round((now - Date.parse(live.since)) / 1000));
    const steps: Step[] = plans.flatMap((p) => p.steps || []);
    return (
        <div className="flex min-w-0 gap-3">
            <Avatar size="sm" initials="S" alt="steve" className="mt-5 shrink-0" />
            <div className="flex min-w-0 max-w-[85%] flex-col gap-1">
                <div className="flex items-center gap-2 text-xs text-quaternary">
                    <span>steve · working</span>
                    <Loading01 className="size-3 animate-spin text-fg-brand-primary" />
                    <span>{elapsed}s</span>
                </div>
                <div className="flex min-w-0 flex-col gap-3 rounded-2xl rounded-tl-sm bg-primary px-4 py-3 shadow-xs ring-1 ring-secondary ring-inset">
                    {steps.length > 0 && (
                        <div className="flex flex-col gap-2">
                            {steps.map((s) => (
                                <div key={s.id} className="flex flex-col gap-1.5">
                                    <div className="flex items-center gap-2 text-sm">
                                        <StateBadge state={s.state} />
                                        <span className="font-medium text-primary">{s.id}</span>
                                        <span className="text-xs text-tertiary">{s.agent || "—"}{s.node ? ` @ ${s.node}` : ""}</span>
                                    </div>
                                    {live.steps[s.id] && <Trace p={live.steps[s.id]} />}
                                </div>
                            ))}
                        </div>
                    )}
                    {live.order.filter((id) => !steps.some((s) => s.id === id)).map((id) => (
                        <div key={id} className="flex flex-col gap-1.5">
                            <div className="text-sm font-medium text-primary">{id}</div>
                            <Trace p={live.steps[id]} />
                        </div>
                    ))}
                    {live.turn && <Trace p={live.turn} showAnswer />}
                    {!live.turn && !live.order.length && steps.length === 0 && (
                        <span className="text-sm text-tertiary">Placing the work…</span>
                    )}
                </div>
            </div>
        </div>
    );
}

// Trace is one agent's progress: its checklist, its reasoning tail, its
// tool calls, and (for a plain turn) the answer forming.
function Trace({ p, showAnswer }: { p: Progress; showAnswer?: boolean }) {
    return (
        <div className="flex min-w-0 flex-col gap-2">
            {(p.agent || p.model) && (
                <div className="text-xs text-quaternary">{[p.agent, p.node, p.model].filter(Boolean).join(" · ")}</div>
            )}
            {p.plan?.length ? (
                <ul className="flex flex-col gap-0.5 text-xs">
                    {p.plan.map((line, i) => (
                        <li key={i} className="flex items-start gap-1.5 text-secondary">
                            {line.status === "completed" ? <CheckCircle className="mt-0.5 size-3 shrink-0 text-fg-success-primary" /> : line.status === "in_progress" ? <Loading01 className="mt-0.5 size-3 shrink-0 animate-spin text-fg-brand-primary" /> : <span className="mt-1 size-2 shrink-0 rounded-full border border-secondary" />}
                            <span className={line.status === "completed" ? "text-tertiary line-through" : ""}>{line.text}</span>
                        </li>
                    ))}
                </ul>
            ) : null}
            {p.reasoning && (
                <div className="max-h-40 overflow-y-auto whitespace-pre-wrap break-words [overflow-wrap:anywhere] rounded-lg bg-secondary px-3 py-2 text-xs italic text-tertiary">{p.reasoning}</div>
            )}
            {p.tools?.length ? <Tools tools={p.tools} /> : null}
            {showAnswer && p.answer && (
                <div className="md prose prose-sm max-w-none break-words [overflow-wrap:anywhere]">
                    <Markdown remarkPlugins={[remarkBreaks]}>{p.answer}</Markdown>
                </div>
            )}
        </div>
    );
}

function Tools({ tools }: { tools: ToolCall[] }) {
    return (
        <ul className="flex flex-col gap-1">
            {tools.map((t, i) => (
                <li key={t.id || i} className="min-w-0">
                    <details className="group">
                        <summary className="flex cursor-pointer list-none items-center gap-2 text-xs">
                            {t.status === "completed" ? <CheckCircle className="size-3.5 shrink-0 text-fg-success-primary" /> : t.status === "failed" ? <XCircle className="size-3.5 shrink-0 text-fg-error-primary" /> : <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" />}
                            <Badge type="modern" size="sm" color="gray">{t.kind || "tool"}</Badge>
                            <span className="truncate text-secondary">{t.name || t.detail || ""}</span>
                            {(t.input || t.output) && <ChevronDown className="size-3 shrink-0 text-quaternary transition group-open:rotate-180" />}
                        </summary>
                        {(t.input || t.output) && (
                            <div className="mt-1 ml-5 flex flex-col gap-1">
                                {t.input && <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words [overflow-wrap:anywhere] rounded-md bg-secondary px-2 py-1 font-mono text-[11px] text-secondary">{t.input}</pre>}
                                {t.output && <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words [overflow-wrap:anywhere] rounded-md bg-secondary px-2 py-1 font-mono text-[11px] text-tertiary">{t.output}</pre>}
                            </div>
                        )}
                    </details>
                </li>
            ))}
        </ul>
    );
}

// ProcessFold is the reply's "how": closed by default, one line saying
// how much there is, the whole trace when opened.
function ProcessFold({ process }: { process: Process }) {
    const steps: StepProcess[] = process.steps || [];
    const count = (process.tools?.length || 0) + steps.reduce((n, s) => n + (s.tools?.length || 0), 0);
    const label = steps.length ? `${steps.length} step${steps.length > 1 ? "s" : ""} · ${count} tool call${count === 1 ? "" : "s"}` : `${count} tool call${count === 1 ? "" : "s"}${process.reasoning ? " · reasoning" : ""}`;
    return (
        <details className="group mt-2 border-t border-secondary pt-2">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs text-tertiary hover:text-primary">
                <ChevronDown className="size-3.5 transition group-open:rotate-180" />
                <span>Process · {label}</span>
            </summary>
            <div className="mt-2 flex flex-col gap-3">
                {steps.map((s) => (
                    <div key={s.id} className="flex flex-col gap-1.5">
                        <div className="text-xs font-medium text-primary">{s.id} <span className="font-normal text-tertiary">{[s.agent, s.node].filter(Boolean).join(" @ ")}</span></div>
                        <Trace p={{ reasoning: s.reasoning, tools: s.tools }} />
                    </div>
                ))}
                {(process.reasoning || process.tools?.length) ? <Trace p={{ reasoning: process.reasoning, tools: process.tools }} /> : null}
            </div>
        </details>
    );
}
