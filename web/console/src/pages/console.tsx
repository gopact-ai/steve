import { useCallback, useEffect, useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkBreaks from "remark-breaks";
import { CheckCircle, ChevronDown, Loading01, MessageChatSquare, Send01, XCircle } from "@untitledui/icons";
import { Avatar } from "@/components/base/avatar/avatar";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { fetchContext, fetchReplies, fetchSuggest, send, when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { ConversationContext, Event, Plan, Process, Progress, Project, Reply, Step, StepProcess, Suggestion, ToolCall } from "@/lib/types";
import { label, zh } from "@/lib/labels";
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
    const [suggestions, setSuggestions] = useState<Suggestion[]>([]);
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

    // Completion comes from the coordinator, by the rules the line will be
    // judged by; the page keeps no rules, only a short debounce.
    useEffect(() => {
        setPick(0);
        const line = text;
        if (!line.trim() || line.includes("\n") || !(line.startsWith("/") || /(^|\s)@[\w-]*$/.test(line))) { setSuggestions([]); return; }
        const t = window.setTimeout(() => {
            void fetchSuggest(conversation, line).then((data) => { if (box.current?.value === line) setSuggestions(data.suggestions || []); }).catch(() => setSuggestions([]));
        }, 120);
        return () => window.clearTimeout(t);
    }, [text, conversation]);
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
            if (e.key === "Escape") { e.preventDefault(); setSuggestions([]); return; }
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
                <span className="text-xs text-tertiary">直接说话就是给当前 Agent 派活；输入 <span className="font-mono">/</span> 看 Steve 能做什么，<span className="font-mono">@</span> 选 Agent。</span>
                <span className="ml-auto text-xs text-tertiary">{status}</span>
            </header>

            <div className="min-h-0 flex-1 overflow-y-auto overflow-x-hidden px-6 py-5">
                {!enabled && <Nothing icon={MessageChatSquare} title="控制台未启用">配置 feishu.owner_open_id：控制台以 owner 身份行事。</Nothing>}
                {enabled && entries.length === 0 && !live && (
                    <Nothing icon={MessageChatSquare} title="这里还没说过话">说你想做的事。输入 / 看动词，@ 选 Agent。</Nothing>
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
                                <div className="border-t border-secondary px-3 py-1 text-[11px] text-quaternary">↑↓ 选择 · Tab 填入 · Enter 发送 · Esc 收起</div>
                            </div>
                        )}
                        <TextArea
                            aria-label="Message"
                            textAreaRef={box}
                            value={text}
                            rows={2}
                            placeholder="说你想做的事。/ 看动词，@ 选 Agent。Enter 发送，Shift+Enter 换行。"
                            onChange={(v) => setText(v)}
                            onKeyDown={onKey}
                            className="flex-1"
                        />
                        <Button size="lg" color="primary" iconTrailing={Send01} isLoading={busy} isDisabled={!text.trim()} onClick={() => void submit()}>
                            发送
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
                <span className="text-quaternary" title="项目决定活在哪台机器的哪个目录里干。切换只影响本会话：已有任务不迁移；当前 Agent 的会话归档并新开；有回合在跑时不能切。">项目</span>
                <div className="w-64">
                    <Select aria-label="Project" size="sm" selectedKey={context.project?.id ?? null} onSelectionChange={(k) => k && String(k) !== context.project?.id && onProject(String(k))} items={projectItems}>
                        {(item) => <Select.Item id={item.id} supportingText={item.supportingText}>{item.label}</Select.Item>}
                    </Select>
                </div>
                {context.project && (
                    <span className="flex items-center gap-1.5">
                        <span className="text-quaternary">项目主机</span><Mono>{context.project.node}</Mono>
                        <span className="text-quaternary">主目录</span><Mono className="text-quaternary">{context.project.path}</Mono>
                        <Badge type="modern" size="sm" color="gray">{context.project.level}</Badge>
                        <Badge type="modern" size="sm" color="gray">{label(zh.repo, context.project.repo)}</Badge>
                        {!context.project.bound && <span className="text-quaternary" title="本会话还没绑定项目；第一条消息时按默认绑定。">（默认）</span>}
                    </span>
                )}
            </div>
            <div className="flex items-center gap-2">
                <span className="text-quaternary" title="当前 Agent 接普通消息。在这里选 = /use 切换当前 Agent；在输入框里 @ 某个 Agent = 只指派下一条消息。">Agent</span>
                <div className="w-64">
                    <Select aria-label="Agent" size="sm" selectedKey={context.agent?.id ?? null} onSelectionChange={(k) => k && String(k) !== context.agent?.id && onAgent(String(k))} items={agentItems}>
                        {(item) => <Select.Item id={item.id} supportingText={item.supportingText} isDisabled={item.isDisabled}>{item.label}</Select.Item>}
                    </Select>
                </div>
                {context.agent && <span>{context.agent.node} · {context.agent.harness}{context.agent.model ? ` · 实际 ${context.agent.model}` : ""} · {context.agent.ready ? <span className="text-success-primary">可用</span> : <span className="text-error-primary" title={context.agent.why}>不可用</span>}</span>}
            </div>
            <div className="flex items-center gap-1.5">
                <span className="text-quaternary">可接本会话</span>
                {usable.length ? usable.map((a) => <Mono key={a.id}>{a.id}</Mono>) : <span className="text-error-primary">没有 — 换一个 Agent 所在机器上的项目</span>}
                {elsewhere.length > 0 && (
                    <span className="ml-2 text-quaternary" title={elsewhere.map((a) => `${a.id}: ${a.because || a.why}`).join("\n")}>
                        不能：{elsewhere.map((a) => a.id).join("、")}
                    </span>
                )}
            </div>
        </div>
    );
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
                    <span className="text-xs text-quaternary">你 · {when(r.at)}</span>
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
                    <span>steve · 进行中</span>
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
                        <span className="text-sm text-tertiary">正在放置…</span>
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
    const label = steps.length ? `${steps.length} 步 · ${count} 次工具调用` : `${count} 次工具调用${process.reasoning ? " · 有推理" : ""}`;
    return (
        <details className="group mt-2 border-t border-secondary pt-2">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs text-tertiary hover:text-primary">
                <ChevronDown className="size-3.5 transition group-open:rotate-180" />
                <span>过程 · {label}</span>
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
