import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router";
import Markdown from "react-markdown";
import remarkBreaks from "remark-breaks";
import { ArrowUp, CheckCircle, ChevronDown, Edit05, Folder, GitBranch01, Loading01, MessageChatSquare, Plus, Square, XCircle } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { Avatar } from "@/components/base/avatar/avatar";
import { Badge } from "@/components/base/badges/badges";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Chips, KeyValue, Panel } from "@/lib/page";
import { fetchContext, fetchConversations, fetchReplies, fetchSuggest, fetchVerbs, send, when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { Conversation, ConversationContext, Event, Injected, Plan, Process, Progress, Project, Reply, Step, StepProcess, Suggestion, Task, ToolCall, Verb } from "@/lib/types";
import { CallGraph } from "@/lib/tree";
import { label, zh } from "@/lib/labels";
import { formatToolText } from "@/lib/tooltext";
import { Mono, Nothing, StateBadge } from "@/lib/ui";


// Live is what the current line is doing: the turn's own progress, and
// each plan step's, until the reply lands.
interface Live { since: string; turn?: Progress; steps: Record<string, Progress>; order: string[] }

export function ConsolePage() {
    const { snap, consoleEvents, refresh } = useFleet();
    const { intent } = useIntent();
    const [conversation, setConversation] = useState(() => sessionStorage.getItem("steve.conversation") || "console:main");
    const [conversations, setConversations] = useState<Conversation[]>([]);
    const [entries, setEntries] = useState<Reply[]>([]);
    const [enabled, setEnabled] = useState(true);
    const [text, setText] = useState("");
    const [busy, setBusy] = useState(false);
    const [status, setStatus] = useState("");
    const [live, setLive] = useState<Live | null>(null);
    const [context, setContext] = useState<ConversationContext | null>(null);
    const [suggestions, setSuggestions] = useState<Suggestion[]>([]);
    const [pick, setPick] = useState(0);
    const [selectedReply, setSelectedReply] = useState<Reply | null>(null);
    const [tab, setTab] = useState<RailTab>("context");
    const [verbs, setVerbs] = useState<Verb[]>([]);
    useEffect(() => { void fetchVerbs().then((d) => setVerbs(d.verbs || [])).catch(() => undefined); }, []);
    const box = useRef<HTMLTextAreaElement>(null);
    const bottom = useRef<HTMLDivElement>(null);
    const seen = useRef(0);
    const handled = useRef(0);

    useEffect(() => { sessionStorage.setItem("steve.conversation", conversation); }, [conversation]);

    // "/console?new=1&project=x" — from the projects page: a fresh thread
    // bound to that project, then the address is cleaned up.
    const location = useLocation();
    const navigate = useNavigate();
    const opened = useRef(false);
    useEffect(() => {
        const params = new URLSearchParams(location.search);
        if (!params.get("new") || opened.current) return;
        opened.current = true;
        const project = params.get("project") || "";
        const id = "console:" + Date.now().toString(36);
        setConversation(id);
        setEntries([]);
        navigate("/console", { replace: true });
        if (project) window.setTimeout(() => { void send(id, `/project use ${project}`).catch(() => undefined).finally(() => { loadContext(); loadConversations(); }); }, 50);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [location.search]);

    const loadContext = useCallback(() => {
        void fetchContext(conversation).then((data) => setContext(data.context ?? null)).catch(() => undefined);
    }, [conversation]);
    const loadConversations = useCallback(() => {
        void fetchConversations().then((data) => setConversations(data.conversations || [])).catch(() => undefined);
    }, []);

    useEffect(() => {
        void (async () => {
            try {
                const data = await fetchReplies(conversation);
                setEnabled(data.enabled);
                setEntries(data.replies || []);
            } catch (e) { setStatus(String(e)); }
        })();
        loadContext();
        loadConversations();
        setSelectedReply(null);
        setLive(null);
    }, [conversation, loadContext, loadConversations]);

    // The context depends on the fleet: an agent coming up changes who can work here.
    useEffect(() => { loadContext(); }, [snap.at, loadContext]);

    useEffect(() => {
        const fresh = consoleEvents.slice(seen.current);
        seen.current = consoleEvents.length;
        if (fresh.some((ev) => ev.kind === "console.sent" || ev.kind === "console.reply")) loadConversations();
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
    }, [consoleEvents, conversation, loadConversations]);

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

    // The composer grows with the text, up to a few lines, like a chat app's.
    useEffect(() => {
        const el = box.current;
        if (!el) return;
        el.style.height = "0px";
        el.style.height = Math.min(el.scrollHeight, 200) + "px";
    }, [text]);

    function newSession(project?: string) {
        const id = "console:" + Date.now().toString(36);
        setConversation(id);
        setEntries([]);
        setText("");
        if (project) window.setTimeout(() => { void send(id, `/project use ${project}`).catch(() => undefined).finally(() => { loadContext(); loadConversations(); }); }, 50);
        window.setTimeout(() => box.current?.focus(), 0);
    }

    async function submit(line?: string) {
        const input = (line ?? text).trim();
        if (!input || busy) return;
        setText("");
        setBusy(true);
        setStatus("");
        try {
            const reply = await send(conversation, input);
            if (reply?.process) setEntries((list) => list.map((x) => (x.at === reply.at && x.kind === "reply" ? reply : x)));
        } catch (e) {
            setStatus(String(e).replace(/^Error: /, ""));
        } finally {
            setBusy(false);
            setLive(null);
            setSelectedReply(null);
            refresh();
            loadContext();
            loadConversations();
            box.current?.focus();
        }
    }

    const settled = ["done", "failed", "skipped", "cancelled"];
    const mineTasks = snap.tasks.filter((t) => t.channel === conversation);
    const runningPlans = snap.plans.filter((p) => {
        const task = mineTasks.find((t) => t.id === p.task_id);
        return task && (p.steps || []).some((s) => !settled.includes(s.state));
    });
    // The roots of this conversation's call graph: its tasks whose parent
    // is not itself one of them.
    const ids = new Set(mineTasks.map((t) => t.id));
    const roots = mineTasks.filter((t) => !t.parent || !ids.has(t.parent)).sort((a, b) => (b.updated_at || "").localeCompare(a.updated_at || "")).slice(0, 8);

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

    const lastWithProcess = [...entries].reverse().find((r) => r.kind === "reply" && (r.process || r.injected));
    const shownProcess = selectedReply ?? lastWithProcess ?? null;
    const current = conversations.find((c) => c.id === conversation);
    const title = current?.title || (entries.find((r) => r.kind === "sent")?.input?.split("\n")[0]) || "新会话";
    const listed = conversations.some((c) => c.id === conversation) ? conversations : [{ id: conversation, title: "新会话", last_at: "", count: 0, running: false, project: context?.project?.id, agent: context?.agent?.id }, ...conversations];

    return (
        <div className="flex h-full min-h-0">
            <Sessions list={listed} projects={snap.projects} current={conversation} onPick={(id) => setConversation(id)} onNew={newSession} />

            <div className="flex min-h-0 min-w-0 flex-1 flex-col">
                <header className="flex items-center gap-3 border-b border-secondary bg-primary px-6 py-2.5">
                    <div className="min-w-0 flex-1 truncate text-sm font-semibold text-primary" title={title}>{title}</div>
                    {context?.project && <Badge type="pill-color" size="sm" color="gray">{context.project.id} · {context.project.node}</Badge>}
                    {context?.agent && <Badge type="pill-color" size="sm" color={context.agent.ready ? "brand" : "error"}>{context.agent.id}{context.agent.model ? " · " + context.agent.model : ""}</Badge>}
                    <span className="text-xs text-tertiary">{status || (live || busy ? "进行中…" : "")}</span>
                </header>

                <div className="grid min-h-0 flex-1 grid-cols-1 xl:grid-cols-[minmax(0,1fr)_380px]">
                    <div className="flex min-h-0 flex-col">
                        <div className="min-h-0 flex-1 overflow-y-auto overflow-x-hidden px-8 py-6">
                            {!enabled && <Nothing icon={MessageChatSquare} title="控制台未启用">配置 feishu.owner_open_id：控制台以 owner 身份行事。</Nothing>}
                            {enabled && entries.length === 0 && !live && (
                                <div className="mx-auto max-w-3xl">
                                    <Nothing icon={MessageChatSquare} title="新会话">
                                        {context?.project ? <>会在 <b>{context.project.id}</b>（{context.project.node}）上由 <b>{context.agent?.id || "默认 Agent"}</b> 来做。想换，用输入框下面的两个选择。</> : "说你想做的事。输入 / 看动词，@ 指派一次。"}
                                    </Nothing>
                                </div>
                            )}
                            <div className="mx-auto flex max-w-4xl flex-col gap-5">
                                {entries.map((r, i) => <Message key={i} r={r} selected={shownProcess === r} onSelect={(r.process || r.injected) ? () => { setSelectedReply(r); setTab("trace"); } : undefined} />)}
                                {live && <Working live={live} plans={runningPlans} compact />}
                                <div ref={bottom} />
                            </div>
                        </div>
                        <div className="bg-primary px-8 pb-5 pt-2">
                            <div className="relative mx-auto max-w-3xl">
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
                                <div className="flex flex-col rounded-2xl bg-primary shadow-xs ring-1 ring-secondary transition focus-within:ring-brand">
                                    <textarea
                                        ref={box}
                                        aria-label="Message"
                                        value={text}
                                        rows={1}
                                        placeholder={busy ? "正在进行…" : "想做什么"}
                                        onChange={(e) => setText(e.target.value)}
                                        onKeyDown={onKey}
                                        className="max-h-[200px] w-full resize-none bg-transparent px-4 pb-1 pt-3 text-sm text-primary outline-none placeholder:text-placeholder"
                                    />
                                    <div className="flex items-center gap-0.5 px-2 pb-2">
                                        <Dropdown.Root>
                                            <AriaButton aria-label="动词" className="flex size-7 items-center justify-center rounded-full text-fg-quaternary outline-none transition hover:bg-secondary hover:text-fg-quaternary_hover">
                                                <Plus className="size-4" />
                                            </AriaButton>
                                            <Dropdown.Popover placement="top start" className="w-80">
                                                <Dropdown.Menu onAction={(k) => { setText(String(k) + " "); box.current?.focus(); }}>
                                                    <Dropdown.Section>
                                                        <Dropdown.SectionHeader className="px-2 py-1 text-[11px] text-quaternary">动词 · 选一个填进输入框</Dropdown.SectionHeader>
                                                        {verbs.map((v) => (
                                                            <Dropdown.Item key={v.command} id={v.command} textValue={v.command}>
                                                                <div className="flex min-w-0 flex-col">
                                                                    <span className="font-mono text-xs text-primary">{v.command} <span className="text-quaternary">{v.args || ""}</span></span>
                                                                    <span className="truncate text-xs text-tertiary">{v.summary}</span>
                                                                </div>
                                                            </Dropdown.Item>
                                                        ))}
                                                    </Dropdown.Section>
                                                </Dropdown.Menu>
                                            </Dropdown.Popover>
                                        </Dropdown.Root>
                                        <Dropdown.Root>
                                            <AriaButton aria-label="项目" className="flex h-7 items-center gap-1.5 rounded-full px-2 text-xs text-tertiary outline-none transition hover:bg-secondary hover:text-secondary">
                                                <Folder className="size-3.5" />
                                                <span>{context?.project?.id || "项目"}</span>
                                                {context?.project && <span className="text-quaternary">{context.project.node}</span>}
                                                <ChevronDown className="size-3 text-fg-quaternary" />
                                            </AriaButton>
                                            <Dropdown.Popover placement="top start" className="w-80">
                                                <Dropdown.Menu onAction={(k) => { if (String(k) !== context?.project?.id) void submit(`/project use ${String(k)}`); }}>
                                                    {snap.projects.map((p) => (
                                                        <Dropdown.Item key={p.id} id={p.id} textValue={p.id} label={p.id} addon={p.node} />
                                                    ))}
                                                </Dropdown.Menu>
                                            </Dropdown.Popover>
                                        </Dropdown.Root>
                                        <span className="flex-1" />
                                        <Dropdown.Root>
                                            <AriaButton aria-label="Agent" className="flex h-7 items-center gap-1.5 rounded-full px-2 text-xs text-secondary outline-none transition hover:bg-secondary">
                                                <span>{context?.agent?.id || "Agent"}</span>
                                                {context?.agent?.model && <span className="text-quaternary">{context.agent.model}</span>}
                                                <ChevronDown className="size-3 text-fg-quaternary" />
                                            </AriaButton>
                                            <Dropdown.Popover placement="top end" className="w-96">
                                                <Dropdown.Menu onAction={(k) => { if (String(k) !== context?.agent?.id) void submit(`/use ${String(k)}`); }}>
                                                    {(context?.agents ?? []).map((a) => (
                                                        <Dropdown.Item key={a.id} id={a.id} textValue={a.id} isDisabled={!a.usable}>
                                                            <div className="flex min-w-0 flex-col">
                                                                <span className="text-sm text-primary">{a.id} <span className="text-xs text-quaternary">{a.node} · {a.harness}{a.model ? " · " + a.model : ""}</span></span>
                                                                {!a.usable && <span className="truncate text-xs text-tertiary">{a.because || a.why}</span>}
                                                            </div>
                                                        </Dropdown.Item>
                                                    ))}
                                                </Dropdown.Menu>
                                            </Dropdown.Popover>
                                        </Dropdown.Root>
                                        {busy ? (
                                            <button type="button" aria-label="停止" title="停止（/cancel）" onClick={() => void send(conversation, "/cancel").catch(() => undefined)}
                                                className="ml-1 flex size-8 items-center justify-center rounded-full bg-secondary text-fg-secondary ring-1 ring-secondary transition hover:bg-tertiary">
                                                <Square className="size-3.5" />
                                            </button>
                                        ) : (
                                            <button type="button" aria-label="发送" title="发送（Enter）" disabled={!text.trim()} onClick={() => void submit()}
                                                className="ml-1 flex size-8 items-center justify-center rounded-full bg-brand-solid text-white transition hover:bg-brand-solid_hover disabled:bg-disabled disabled:text-fg-disabled">
                                                <ArrowUp className="size-4" />
                                            </button>
                                        )}
                                    </div>
                                </div>
                            </div>
                        </div>
                    </div>
                    <Rail context={context} live={live} plans={runningPlans} reply={shownProcess} tab={tab} setTab={setTab} roots={roots} />
                </div>
            </div>
        </div>
    );
}

// Sessions is the left column, a tree: each project is a node that folds,
// with the threads under it and a way to start one there; Steve's home
// sits apart at the bottom. A thread whose project is not known any more
// goes under 未归属.
function Sessions({ list, projects, current, onPick, onNew }: { list: Conversation[]; projects: Project[]; current: string; onPick: (id: string) => void; onNew: (project?: string) => void }) {
    const [folded, setFolded] = useState<Record<string, boolean>>(() => { try { return JSON.parse(localStorage.getItem("steve.folded") || "{}"); } catch { return {}; } });
    const toggle = (id: string) => setFolded((f) => { const next = { ...f, [id]: !f[id] }; try { localStorage.setItem("steve.folded", JSON.stringify(next)); } catch { /* ignore */ } return next; });
    const byProject = new Map<string, Conversation[]>();
    for (const c of list) {
        const key = c.project || "";
        byProject.set(key, [...(byProject.get(key) || []), c]);
    }
    const home = projects.find((p) => p.home);
    const work = projects.filter((p) => !p.home).sort((a, b) => (a.default ? -1 : b.default ? 1 : a.id.localeCompare(b.id)));
    const known = new Set(projects.map((p) => p.id));
    const orphans = [...byProject.entries()].filter(([k]) => !known.has(k)).flatMap(([, v]) => v);
    const node = (p: Project, title: string, hint?: string) => {
        const threads = byProject.get(p.id) || [];
        const open = !folded[p.id];
        const holdsCurrent = threads.some((c) => c.id === current);
        return (
            <li key={p.id} className="flex flex-col">
                <div className={`group flex items-center gap-1 rounded-lg px-1.5 py-1 ${holdsCurrent && !open ? "bg-primary/60" : ""}`}>
                    <button type="button" onClick={() => toggle(p.id)} className="flex size-5 shrink-0 items-center justify-center rounded text-fg-quaternary hover:bg-primary/60" aria-label={open ? "折叠" : "展开"}>
                        <ChevronDown className={`size-3.5 transition ${open ? "" : "-rotate-90"}`} />
                    </button>
                    <Folder className="size-4 shrink-0 text-fg-quaternary" />
                    <button type="button" onClick={() => toggle(p.id)} className="min-w-0 flex-1 truncate text-left text-sm text-primary" title={hint || `${p.node} · ${p.path}`}>{title}</button>
                    {threads.some((c) => c.running) && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                    <button type="button" onClick={() => onNew(p.id)} className="flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary opacity-0 transition hover:bg-primary/60 hover:text-fg-quaternary_hover group-hover:opacity-100" aria-label={`在 ${p.id} 下新会话`} title={`在 ${p.id} 下新会话`}>
                        <Plus className="size-3.5" />
                    </button>
                </div>
                {open && (
                    <ul className="ml-4 flex flex-col gap-0.5 border-l border-secondary pl-2">
                        {threads.length === 0 && <li className="px-2 py-1 text-[11px] text-quaternary">还没有会话</li>}
                        {threads.map((c) => <Thread key={c.id} c={c} current={c.id === current} onPick={onPick} />)}
                    </ul>
                )}
            </li>
        );
    };
    return (
        <aside className="hidden w-72 shrink-0 flex-col border-r border-secondary bg-secondary lg:flex">
            <div className="px-2 pt-3 pb-1">
                <button type="button" onClick={() => onNew()} className="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-primary transition hover:bg-primary/70" title="在当前项目下开一条新会话">
                    <Edit05 className="size-4 text-fg-quaternary" />
                    <span>新会话</span>
                </button>
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
                <div className="px-2 pt-2 pb-1 text-[11px] font-medium uppercase tracking-wide text-quaternary">项目</div>
                <ul className="flex flex-col gap-0.5">
                    {work.map((p) => node(p, p.id + (p.default ? " · 默认" : "")))}
                </ul>
                {orphans.length > 0 && (
                    <>
                        <div className="px-2 pt-3 pb-1 text-[11px] font-medium uppercase tracking-wide text-quaternary">未归属</div>
                        <ul className="ml-2 flex flex-col gap-0.5">{orphans.map((c) => <Thread key={c.id} c={c} current={c.id === current} onPick={onPick} />)}</ul>
                    </>
                )}
                {home && (
                    <>
                        <div className="px-2 pt-3 pb-1 text-[11px] font-medium uppercase tracking-wide text-quaternary">Steve 的家</div>
                        <ul className="flex flex-col gap-0.5">{node(home, "私聊 · " + home.id, "你和 Steve 的私聊默认在这里；放身份与记忆，不是代码。")}</ul>
                    </>
                )}
            </div>
        </aside>
    );
}

function Thread({ c, current, onPick }: { c: Conversation; current: boolean; onPick: (id: string) => void }) {
    return (
        <li>
            <button type="button" onClick={() => onPick(c.id)}
                className={`flex w-full flex-col gap-0.5 rounded-lg px-2 py-1.5 text-left transition ${current ? "bg-primary" : "hover:bg-primary/50"}`}>
                <span className="flex items-center gap-1.5">
                    {c.running && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                    <span className="truncate text-sm text-primary">{c.title || "新会话"}</span>
                </span>
                <span className="truncate text-[11px] text-tertiary">{c.agent || "默认 Agent"}{c.last_at ? ` · ${ago(c.last_at)}` : " · 未开始"}</span>
            </button>
        </li>
    );
}

function ago(at: string): string {
    const s = Math.max(0, Math.round((Date.now() - Date.parse(at)) / 1000));
    if (s < 60) return "刚刚";
    if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
    if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
    return `${Math.floor(s / 86400)} 天前`;
}

type RailTab = "context" | "trace" | "graph";

function Rail({ context, live, plans, reply, tab, setTab, roots }: { context: ConversationContext | null; live: Live | null; plans: Plan[]; reply: Reply | null; tab: RailTab; setTab: (t: RailTab) => void; roots: Task[] }) {
    const { snap } = useFleet();
    // The trace tab takes over while something runs, and returns to
    // context when the user asks.
    useEffect(() => { if (live) setTab("trace"); }, [live, setTab]);
    const usable = context?.agents.filter((a) => a.usable) ?? [];
    const elsewhere = context?.agents.filter((a) => !a.usable) ?? [];
    return (
        <aside className="hidden min-h-0 flex-col border-l border-secondary bg-secondary xl:flex">
            <div className="border-b border-secondary bg-primary px-4 py-2">
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as RailTab)}>
                    <TabList type="button-border" size="sm" items={[{ id: "context", label: "会话" }, { id: "trace", label: live ? "过程（进行中）" : "过程" }, { id: "graph", label: roots.length ? `关系 (${roots.length})` : "关系" }]}>{(item) => <Tab {...item} />}</TabList>
                </Tabs>
            </div>
            <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
                {tab === "context" && context && (
                    <>
                        <Panel title="项目" badge={context.project && !context.project.bound ? <Badge type="pill-color" size="sm" color="gray">默认，未绑定</Badge> : undefined}>
                            {context.project ? (
                                <KeyValue dense rows={[
                                    { k: "名字", v: <span className="font-medium">{context.project.id}</span> },
                                    { k: "项目主机", v: <Mono>{context.project.node}</Mono> },
                                    { k: "主目录", v: <Mono className="text-secondary">{context.project.path}</Mono> },
                                    { k: "工作方式", v: label(zh.repo, context.project.repo), hint: "直接修改主目录：只有项目主机上的 Agent 能接。隔离副本：计划在别的机器上物化副本，完成后合并。" },
                                    { k: "数据等级", v: context.project.level },
                                ]} />
                            ) : <span className="text-sm text-quaternary">没有项目</span>}
                            <p className="text-xs text-quaternary">切换项目只影响本会话：已有任务不迁移，当前 Agent 的会话归档并新开，有回合在跑时不能切。</p>
                        </Panel>
                        <Panel title="当前 Agent">
                            {context.agent ? (
                                <KeyValue dense rows={[
                                    { k: "名字", v: <span className="font-medium">{context.agent.id}</span> },
                                    { k: "机器", v: <Mono>{context.agent.node}</Mono> },
                                    { k: "AI 工具", v: context.agent.harness },
                                    { k: "实际模型", v: context.agent.model || <span className="text-quaternary">未观测到</span> },
                                    { k: "状态", v: context.agent.ready ? <span className="text-success-primary">可用</span> : <span className="text-error-primary">不可用 · {context.agent.why}</span> },
                                ]} />
                            ) : <span className="text-sm text-quaternary">没有当前 Agent</span>}
                        </Panel>
                        <Panel title="谁能接本会话">
                            <div className="flex flex-col gap-2 text-sm">
                                <Chips items={usable.map((a) => ({ id: a.id, title: `${a.node} · ${a.harness}` }))} empty={<span className="text-error-primary">没有 — 换一个 Agent 所在机器上的项目</span>} />
                                {elsewhere.length > 0 && (
                                    <ul className="flex flex-col gap-1 text-xs text-tertiary">
                                        {elsewhere.map((a) => <li key={a.id}><Mono className="text-quaternary">{a.id}</Mono> <span>{a.because || a.why}</span></li>)}
                                    </ul>
                                )}
                            </div>
                        </Panel>
                    </>
                )}
                {tab === "trace" && (
                    live ? <Working live={live} plans={plans} /> : (reply?.process || reply?.injected) ? (
                        <>
                            {reply.injected && <InjectedPanel at={reply.at} in={reply.injected} />}
                            {reply.process && (
                                <Panel title={`过程 · ${when(reply.at)}`}>
                                    <ProcessBody process={reply.process} />
                                </Panel>
                            )}
                        </>
                    ) : <Nothing icon={MessageChatSquare} title="还没有过程">发一条消息，这里会显示给 agent 的上下文、它的推理、工具调用和步骤。</Nothing>
                )}
                {tab === "graph" && (
                    roots.length ? (
                        <Panel title="谁在为这条会话干活" badge={<span className="text-xs text-tertiary">任务 → 步骤 / 委派</span>}>
                            <CallGraph roots={roots} tasks={snap.tasks} plans={snap.plans} liveSteps={live?.order} />
                        </Panel>
                    ) : <Nothing icon={GitBranch01} title="还没有任务">这条会话的任务、它拆出的步骤、以及 Agent 之间的委派会画在这里。</Nothing>
                )}
            </div>
        </aside>
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

function Message({ r, selected, onSelect }: { r: Reply; selected?: boolean; onSelect?: () => void }) {
    if (r.kind === "sent") {
        return (
            <div className="flex justify-end">
                <div className="max-w-[75%] rounded-2xl bg-secondary px-4 py-2.5 text-sm text-primary whitespace-pre-wrap break-words [overflow-wrap:anywhere]">{r.input}</div>
            </div>
        );
    }
    const tone = r.error ? "error" : r.kind === "milestone" ? "success" : r.kind === "notice" ? "warning" : "gray";
    return (
        <div className={`flex min-w-0 flex-col gap-1 rounded-xl px-2 py-1 ${selected ? "bg-secondary/60" : ""}`}>
            {r.title && <div className="text-sm font-semibold text-primary">{r.title}</div>}
            <div className={`md prose prose-sm max-w-none break-words [overflow-wrap:anywhere] ${r.error ? "text-error-primary" : ""}`}>
                <Markdown remarkPlugins={[remarkBreaks]}>{r.text}</Markdown>
            </div>
            <div className="flex items-center gap-2 text-[11px] text-quaternary">
                <span>{when(r.at)}</span>
                {r.kind !== "reply" && <Badge type="pill-color" size="sm" color={tone}>{r.kind}</Badge>}
                {r.error && <Badge type="pill-color" size="sm" color="error">error</Badge>}
                {onSelect && <button type="button" onClick={onSelect} className="hover:text-primary">过程</button>}
            </div>
            {r.process && <div className="xl:hidden"><ProcessFold process={r.process} /></div>}
        </div>
    );
}

// InjectedPanel is what the agent was given for a turn: where it worked,
// as whom, with which model and options, on a new or resumed session,
// which MCP servers were attached, and — the first turn of a session —
// the assembled instructions it read before the prompt.
function InjectedPanel({ at, in: x }: { at: string; in: Injected }) {
    const opts = Object.entries(x.options || {});
    return (
        <Panel title={`给 agent 的 · ${when(at)}`} badge={<span className="text-xs text-tertiary">{x.new_session ? "新会话" : "续用会话"}</span>}>
            <KeyValue dense rows={[
                { k: "Agent", v: <span>{x.agent} <span className="text-tertiary">· {x.harness}{x.node ? " @ " + x.node : ""}</span></span> },
                { k: "项目", v: x.project ? <span>{x.project} <Mono className="text-tertiary">{x.workspace}</Mono></span> : <span className="text-quaternary">—</span> },
                { k: "模型", v: x.model ? <span>固定为 {x.model}</span> : <span className="text-quaternary">未固定，AI 工具默认</span> },
                ...(opts.length ? [{ k: "选项", v: <span>{opts.map(([k, v]) => `${k}=${v}`).join(" · ")}</span> }] : []),
                { k: "MCP", v: x.mcp_servers?.length ? <Chips items={x.mcp_servers.map((m) => ({ id: m }))} /> : <span className="text-quaternary">无</span> },
                { k: "指令", v: x.instructions_sent ? <span>本轮发送，{(x.instructions_bytes / 1024).toFixed(1)} KB（身份 + 技能 + 记忆）</span> : <span className="text-tertiary">会话开头已发过，本轮未重发（{(x.instructions_bytes / 1024).toFixed(1)} KB）</span>, hint: "拼装好的指令只在会话的第一轮放在 prompt 前面；之后的回合 AI 工具靠自己的会话记忆。" },
                { k: "会话", v: x.session ? <Mono className="text-tertiary">{x.session.slice(0, 24)}</Mono> : <span className="text-quaternary">—</span> },
                ...(x.fingerprint ? [{ k: "指纹", v: <Mono className="text-quaternary">{x.fingerprint.slice(0, 16)}</Mono>, hint: "指令 + MCP + 技能的摘要；变了会提示 /new。" }] : []),
            ]} />
            {x.prompt && (
                <details className="mt-2 text-xs">
                    <summary className="cursor-pointer text-tertiary hover:text-primary">本轮发给它的 prompt（{x.prompt.length} 字）</summary>
                    <pre className="mt-1 max-h-64 overflow-auto whitespace-pre-wrap break-words [overflow-wrap:anywhere] rounded-md bg-secondary px-2.5 py-1.5 font-mono text-[11px] leading-relaxed text-secondary">{x.prompt}</pre>
                </details>
            )}
            {x.instructions && (
                <details className="mt-1 text-xs">
                    <summary className="cursor-pointer text-tertiary hover:text-primary">指令全文（{(x.instructions_bytes / 1024).toFixed(1)} KB）</summary>
                    <pre className="mt-1 max-h-96 overflow-auto whitespace-pre-wrap break-words [overflow-wrap:anywhere] rounded-md bg-secondary px-2.5 py-1.5 font-mono text-[11px] leading-relaxed text-secondary">{x.instructions}</pre>
                </details>
            )}
        </Panel>
    );
}

// Working is the bubble for the line in flight: plan steps with their
// states, and under each the agent's reasoning and tool calls as they
// happen; or, for a plain turn, the agent's own.
function Working({ live, plans, compact }: { live: Live; plans: Plan[]; compact?: boolean }) {
    const [now, setNow] = useState(Date.now());
    useEffect(() => { const t = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(t); }, []);
    const elapsed = Math.max(0, Math.round((now - Date.parse(live.since)) / 1000));
    const steps: Step[] = plans.flatMap((p) => p.steps || []);
    const latest = live.turn ?? (live.order.length ? live.steps[live.order[live.order.length - 1]] : undefined);
    const lastTool = latest?.tools?.length ? latest.tools[latest.tools.length - 1] : undefined;
    if (compact) {
        // In the transcript, one line: the rail has the detail on wide
        // screens, and the fold appears below on narrow ones.
        return (
            <div className="flex min-w-0 gap-3">
                <Avatar size="sm" initials="S" alt="steve" className="mt-5 shrink-0" />
                <div className="flex min-w-0 max-w-[85%] flex-col gap-1">
                    <div className="flex items-center gap-2 text-xs text-quaternary">
                        <span>steve · 进行中</span>
                        <Loading01 className="size-3 animate-spin text-fg-brand-primary" />
                        <span>{elapsed}s</span>
                    </div>
                    <div className="flex min-w-0 flex-col gap-2 rounded-2xl rounded-tl-sm bg-primary px-4 py-3 shadow-xs ring-1 ring-secondary ring-inset">
                        <div className="truncate text-sm text-secondary">
                            {latest ? [latest.agent, latest.model].filter(Boolean).join(" · ") : "正在放置…"}
                            {lastTool ? <span className="text-tertiary"> · {lastTool.kind} {lastTool.name}</span> : null}
                        </div>
                        {latest?.answer && <div className="md prose prose-sm max-w-none break-words [overflow-wrap:anywhere]"><Markdown remarkPlugins={[remarkBreaks]}>{latest.answer}</Markdown></div>}
                        <div className="xl:hidden"><Trace p={latest ?? {}} /></div>
                    </div>
                </div>
            </div>
        );
    }
    return (
        <Panel title="进行中" badge={<span className="flex items-center gap-1 text-xs text-tertiary"><Loading01 className="size-3 animate-spin text-fg-brand-primary" />{elapsed}s</span>}>
            <div className="flex min-w-0 flex-col gap-3">
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
        </Panel>
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
                <div className="flex flex-col gap-1 rounded-lg bg-secondary px-3 py-2">
                    <div className="text-[11px] text-quaternary" title="AI 工具在每一步之前给出的一句话概要；它不暴露完整的思考过程。">思考摘要</div>
                    <div className="md prose prose-sm max-h-40 max-w-none overflow-y-auto break-words text-xs text-tertiary [overflow-wrap:anywhere] prose-p:my-0.5 prose-strong:font-medium prose-strong:text-secondary">
                        <Markdown remarkPlugins={[remarkBreaks]}>{p.reasoning}</Markdown>
                    </div>
                </div>
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
                            <div className="mt-1 ml-5 flex flex-col gap-1.5">
                                <ToolText label="输入" raw={t.input} />
                                <ToolText label="输出" raw={t.output} muted />
                            </div>
                        )}
                    </details>
                </li>
            ))}
        </ul>
    );
}

// ToolText shows one side of a tool call as a person would read it: a
// shell line as a shell line, an output envelope as its text, JSON as
// indented JSON. The exit code and other scalars become a small meta line.
function ToolText({ label: name, raw, muted }: { label: string; raw?: string; muted?: boolean }) {
    const shown = formatToolText(raw);
    if (!shown) return null;
    return (
        <div className="flex flex-col gap-0.5">
            <div className="flex items-center gap-2 text-[11px] text-quaternary">
                <span>{name}</span>
                {shown.lang === "shell" && <span className="font-mono">$</span>}
                {shown.meta && <span>{shown.meta}</span>}
            </div>
            <pre className={`max-h-64 overflow-auto whitespace-pre-wrap break-words [overflow-wrap:anywhere] rounded-md bg-secondary px-2.5 py-1.5 font-mono text-[11px] leading-relaxed ${muted ? "text-tertiary" : "text-secondary"}`}>{shown.body}</pre>
        </div>
    );
}

// ProcessBody is a reply's trace: each step's, then the turn's own.
function ProcessBody({ process }: { process: Process }) {
    const steps: StepProcess[] = process.steps || [];
    return (
        <div className="flex flex-col gap-3">
            {steps.map((s) => (
                <div key={s.id} className="flex flex-col gap-1.5">
                    <div className="text-xs font-medium text-primary">{s.id} <span className="font-normal text-tertiary">{[s.agent, s.node].filter(Boolean).join(" @ ")}</span></div>
                    <Trace p={{ reasoning: s.reasoning, tools: s.tools }} />
                </div>
            ))}
            {(process.reasoning || process.tools?.length) ? <Trace p={{ reasoning: process.reasoning, tools: process.tools }} /> : null}
        </div>
    );
}

// ProcessFold is the reply's "how" on a narrow screen: closed by default,
// one line saying how much there is, the whole trace when opened.
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
            <div className="mt-2"><ProcessBody process={process} /></div>
        </details>
    );
}
