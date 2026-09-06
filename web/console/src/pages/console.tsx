import { useCallback, useEffect, useLayoutEffect, useRef, useState, type SetStateAction } from "react";
import { useLocation, useNavigate } from "react-router";
import { MessageChatSquare } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Composer, type Queued } from "@/components/steve/composer";
import { AssistantMessage, UserMessage } from "@/components/steve/message";
import { Rail, type RailTab } from "@/components/steve/rail";
import { SessionsTree } from "@/components/steve/sessions-tree";
import { TaskDrawer } from "@/components/steve/task-drawer";
import { DelegationCard } from "@/components/steve/delegation";
import { RAIL_WIDTH } from "@/components/steve/rail";
import { BoardPage } from "./board";
import { Working, applyLive, type Live } from "@/components/steve/trace";
import { Nothing } from "@/components/steve/ui";
import { enqueue, fetchQueue, deleteQueued, editQueued, steerQueued, fetchContext, fetchConversations, fetchReplies, fetchSuggest, fetchVerbs, send, updateConversation, fetchSelectors, setPreferences } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import { applyDelegation, restoreDelegations, withDelegations, type Delegations } from "@/lib/delegations";
import type { Conversation, ConversationContext, Reply, Suggestion, Verb, Task, StepProcess, QuoteRef, Exchange } from "@/lib/types";

function readDraft(id: string): string {
    try { return sessionStorage.getItem(`steve.draft.${id}`) || ""; } catch { return ""; }
}

// ConsolePage is composition: it owns the conversation, the transcript,
// the line in flight and the composer's text, and lays out the three
// columns from components/steve. Nothing here draws.
export function ConsolePage() {
    const { snap, consoleEvents, refresh, live: connection, hubUpdated } = useFleet();
    const { intent } = useIntent();
    const [conversation, setConversation] = useState(() => sessionStorage.getItem("steve.conversation") || "console:main");
    const [conversations, setConversations] = useState<Conversation[]>([]);
    const [entries, setEntries] = useState<Reply[]>([]);
    const [enabled, setEnabled] = useState(true);
    const [drafts, setDrafts] = useState<Record<string, string>>({});
    const draftValues = useRef(drafts);
    const text = drafts[conversation] ?? readDraft(conversation);
    function writeDraft(id: string, value: SetStateAction<string>) {
        const current = draftValues.current[id] ?? readDraft(id);
        const next = typeof value === "function" ? value(current) : value;
        draftValues.current = { ...draftValues.current, [id]: next };
        try { sessionStorage.setItem(`steve.draft.${id}`, next); }
        catch { setStatus("草稿暂时无法保存，请保留当前页面并复制重要内容"); }
        setDrafts(draftValues.current);
    }
    const setText = (value: SetStateAction<string>) => writeDraft(conversation, value);
    useEffect(() => {
        if (hubUpdated && text === "") window.location.reload();
    }, [hubUpdated, text]);
    const [sending, setSending] = useState<Record<string, boolean>>({});
    const submitting = useRef(new Set<string>());
    const [stopping, setStopping] = useState<Record<string, boolean>>({});
    const stopRequests = useRef(new Set<string>());
    const [creating, setCreating] = useState(false);
    const creatingRequest = useRef(false);
    const [status, setStatus] = useState("");
    const [live, setLive] = useState<Live | null>(null);
    const [delegations, setDelegations] = useState<Delegations>({});
    const transcript = entries.map((r) => withDelegations(r, delegations));
    const [context, setContext] = useState<ConversationContext | null>(null);
    const [suggestions, setSuggestions] = useState<Suggestion[]>([]);
    const [pick, setPick] = useState(0);
    const [selectedReply, setSelectedReply] = useState<Reply | null>(null);
    const [tab, setTab] = useState<RailTab>("context");
    const [pickedTask, setPickedTask] = useState<Task | null>(null);
    // A delegated child picked from the tree takes the middle column:
    // its card, open, from the reply that carried it.
    const [child, setChild] = useState<Task | null>(null);
    const stepOf = (taskID: string): StepProcess | undefined => {
        if (delegations["#" + taskID]) return delegations["#" + taskID].step;
        for (let i = entries.length - 1; i >= 0; i--) {
            const found = entries[i].process?.steps?.find((st) => st.id === "#" + taskID);
            if (found) return found;
        }
        return undefined;
    };
    // The server owns execution; every tab projects the same durable queue.
    const [exchanges, setExchanges] = useState<Exchange[]>([]);
    const queue = exchanges.filter((e) => e.conversation === conversation && e.state === "queued");
    const busy = !!live || exchanges.some((e) => e.conversation === conversation && e.state === "running");
    const activeConversation = useRef(conversation);
    activeConversation.current = conversation;
    const queueRequest = useRef(0);
    const transcriptRequest = useRef(0);
    // Quotes ride with the next message wherever it is sent from; they
    // survive switching threads on purpose — that is how a line from one
    // thread reaches another.
    const [quotes, setQuotes] = useState<QuoteRef[]>([]);
    const [queueing, setQueueingState] = useState<boolean>(() => { try { return localStorage.getItem("steve.queueing") !== "0"; } catch { return true; } });
    const setQueueing = (v: boolean) => { setQueueingState(v); try { localStorage.setItem("steve.queueing", v ? "1" : "0"); } catch { /* ignore */ } };
    const [sessionsCollapsed, setSessionsCollapsedState] = useState<boolean>(() => { try { return localStorage.getItem("steve.sessions.collapsed") === "1"; } catch { return false; } });
    const setSessionsCollapsed = (v: boolean) => { setSessionsCollapsedState(v); try { localStorage.setItem("steve.sessions.collapsed", v ? "1" : "0"); } catch { /* ignore */ } };
    const [verbs, setVerbs] = useState<Verb[]>([]);
    useEffect(() => { void fetchVerbs().then((d) => setVerbs(d.verbs || [])).catch(() => undefined); }, []);
    const box = useRef<HTMLTextAreaElement>(null);
    const transcriptBox = useRef<HTMLDivElement>(null);
    const followTranscript = useRef(true);
    const seen = useRef(0);
    const handled = useRef(0);

    useEffect(() => { sessionStorage.setItem("steve.conversation", conversation); }, [conversation]);

    // "/console?new=1&project=x" — from the projects page: a fresh thread
    // bound to that project, then the address is cleaned up.
    const location = useLocation();
    const view = new URLSearchParams(location.search).get("view") === "board" ? "board" : "chat";
    const navigate = useNavigate();
    const opened = useRef(false);
    useEffect(() => {
        const params = new URLSearchParams(location.search);
        if (!params.get("new") || opened.current) return;
        opened.current = true;
        const project = params.get("project") || undefined;
        navigate("/console", { replace: true });
        void newSession(project);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [location.search]);

    useEffect(() => {
        const id = new URLSearchParams(location.search).get("conversation");
        if (!id?.startsWith("console:")) return;
        selectConversation(id);
        navigate("/console", { replace: true });
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [location.search]);

    const loadContext = useCallback(() => {
        void fetchContext(conversation).then((data) => { if (activeConversation.current === conversation) setContext(data.context ?? null); }).catch(() => undefined);
    }, [conversation]);
    const loadConversations = useCallback(() => {
        void fetchConversations().then((data) => setConversations(data.conversations || [])).catch(() => undefined);
    }, []);

    const loadQueue = useCallback(async () => {
        if (activeConversation.current !== conversation) return;
        const request = ++queueRequest.current;
        try {
            const data = await fetchQueue(conversation);
            if (activeConversation.current !== conversation || request !== queueRequest.current) return;
            setExchanges(data.queue || []);
            const running = [...(data.queue || [])].filter((e) => e.state === "running").sort((a, b) => (b.started_at || "").localeCompare(a.started_at || ""))[0];
            setLive((cur) => running ? (cur?.exchangeID === running.id ? cur : { since: running.started_at || running.enqueued_at, exchangeID: running.id, steps: {}, order: [] }) : null);
        } catch (e) {
            if (activeConversation.current === conversation) setStatus(String(e).replace(/^Error: /, ""));
        }
    }, [conversation]);
    const loadReplies = useCallback(async () => {
        if (activeConversation.current !== conversation) return;
        const request = ++transcriptRequest.current;
        const cursor = seen.current;
        try {
            const data = await fetchReplies(conversation);
            if (activeConversation.current !== conversation || request !== transcriptRequest.current) return;
            setEnabled(data.enabled);
            setEntries(data.replies || []);
            setDelegations((cur) => restoreDelegations(cur, data.replies || [], cursor));
        } catch (e) {
            if (activeConversation.current === conversation) setStatus(String(e));
        }
    }, [conversation]);

    useEffect(() => {
        setEntries([]);
        setDelegations({});
        setExchanges([]);
        void loadReplies();
        void loadQueue();
        loadContext();
        loadConversations();
        setSelectedReply(null);
        setLive(null);
        setChild(null);
    }, [conversation, loadContext, loadConversations, loadQueue, loadReplies]);

    // Recover after reconnecting or missing an SSE event, including a turn
    // completed while this tab was asleep. Fetches never submit work.
    useEffect(() => {
        if (connection === "live") { void loadQueue(); void loadReplies(); loadConversations(); }
        const timer = window.setInterval(() => { void loadQueue(); void loadReplies(); loadConversations(); }, 10000);
        return () => window.clearInterval(timer);
    }, [connection, loadQueue, loadReplies, loadConversations]);

    // The context depends on the fleet: an agent coming up changes who can work here.
    useEffect(() => { loadContext(); }, [snap.at, loadContext]);

    useEffect(() => {
        // The buffer is trimmed from the front, so the cursor is the
        // arrival number of the last event handled, not an index.
        const fresh = consoleEvents.filter((ev) => (ev.n ?? 0) > seen.current);
        if (fresh.length) seen.current = fresh[fresh.length - 1].n ?? seen.current;
        if (fresh.some((ev) => ev.kind === "console.sent" || ev.kind === "console.reply" || ev.kind === "console.meta" || ev.kind === "console.queue")) loadConversations();
        const mine = fresh.filter((ev) => ev.conversation === conversation);
        if (!mine.length) return;
        setLive((cur) => mine.reduce(applyLive, cur));
        setDelegations((cur) => mine.reduce(applyDelegation, cur));
        if (mine.some((ev) => ev.kind === "console.queue")) void loadQueue();
        if (mine.some((ev) => ev.kind === "console.sent" || ev.kind === "console.reply")) void loadReplies();
        if (mine.some((ev) => ev.kind === "console.reply")) { loadContext(); refresh(); }
        const lines = mine.filter((ev) => ["console.sent", "console.reply", "console.notice", "console.milestone"].includes(ev.kind));
        if (!lines.length) return;
        setEntries((list) => {
            const next = [...list];
            for (const ev of lines) {
                const kind = ev.kind.slice("console.".length);
                const r: Reply = { id: ev.reply_id, exchange_id: ev.exchange_id, at: ev.at, conversation, kind, title: ev.title, text: kind === "sent" ? "" : ev.text || "", input: kind === "sent" ? ev.text : undefined };
                const index = next.findIndex((x) => r.id ? x.id === r.id : r.exchange_id ? x.exchange_id === r.exchange_id && x.kind === r.kind : x.at === r.at && x.kind === r.kind);
                if (index < 0) next.push(r);
                else next[index] = { ...next[index], ...r };
            }
            return next.slice(-200);
        });
    }, [consoleEvents, conversation, loadConversations, loadQueue, loadReplies, loadContext, refresh]);

    useLayoutEffect(() => {
        if (followTranscript.current && transcriptBox.current) {
            transcriptBox.current.scrollTop = transcriptBox.current.scrollHeight;
        }
    }, [entries, live, view, child]);

    useEffect(() => {
        if (!intent || intent.n === handled.current) return;
        if (intent.mode === "run" && !context) return;
        handled.current = intent.n;
        if (intent.mode === "fill") { setText(intent.text + " "); box.current?.focus(); }
        else void submit(intent.text);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [intent, context]);

    // The composer grows with the text, up to a few lines, like a chat app's.
    useEffect(() => {
        const el = box.current;
        if (!el) return;
        el.style.height = "0px";
        el.style.height = Math.min(el.scrollHeight, 200) + "px";
    }, [text]);

    function selectConversation(id: string, nextContext: ConversationContext | null = null) {
        if (id === activeConversation.current) return;
        activeConversation.current = id;
        followTranscript.current = true;
        setConversation(id);
        setEntries([]);
        setExchanges([]);
        setContext(nextContext);
        setLive(null);
        setChild(null);
        setPickedTask(null);
        setSelectedReply(null);
        setStatus("");
        window.setTimeout(() => box.current?.focus(), 0);
    }

    async function newSession(project = context?.project?.id) {
        if (creatingRequest.current) return;
        creatingRequest.current = true;
        setCreating(true);
        setStatus("");
        const from = conversation;
        const id = `console:${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
        try {
            if (!project) throw new Error("项目尚未就绪，请稍后再创建会话");
            await send(id, `/project use ${project}`);
            const data = await fetchContext(id);
            if (data.context?.project?.id !== project || !data.context.project.bound) throw new Error("新会话未能绑定项目，请重试");
            // Do not pull the reader out of a different thread chosen while binding.
            if (activeConversation.current === from) selectConversation(id, data.context);
            loadConversations();
        } catch (e) {
            if (activeConversation.current === from) setStatus(String(e).replace(/^Error: /, ""));
        } finally {
            creatingRequest.current = false;
            setCreating(false);
        }
    }

    async function queueAction(action: () => Promise<unknown>) {
        try {
            await action();
            setStatus("");
        } catch (e) {
            if (activeConversation.current === conversation) setStatus(String(e).replace(/^Error: /, ""));
        } finally { await loadQueue(); }
    }

    function steer(q: Queued) { void queueAction(() => steerQueued(q.id)); }

    async function sideChat(q: Queued) {
        const id = "console:" + Date.now().toString(36);
        await queueAction(async () => {
            // Finish binding before submitting: a timer can race a slow hub.
            if (context?.project?.id) await send(id, `/project use ${context.project.id}`);
            await deleteQueued(q.id);
            try { await enqueue(id, q.input, q.quotes); }
            catch (e) { writeDraft(id, q.input); setQuotes(q.quotes || []); selectConversation(id); throw e; }
            selectConversation(id);
        });
    }

    async function submit(line?: string) {
        const input = (line ?? text).trim();
        if (!input || submitting.current.has(conversation) || creatingRequest.current || !context) return;
        if (busy && !queueing) { setStatus("当前回合进行中，排队已关闭"); return; }
        submitting.current.add(conversation);
        setSending((all) => ({ ...all, [conversation]: true }));
        setStatus("");
        const carried = quotes;
        if (line === undefined) setText("");
        setQuotes((list) => list.filter((q) => !carried.includes(q)));
        followTranscript.current = true;
        try {
            await enqueue(conversation, input, carried.length ? carried : undefined);
            if (activeConversation.current === conversation) {
                setSelectedReply(null);
            }
        } catch (e) {
            if (line === undefined) writeDraft(conversation, (current) => current ? `${input}\n${current}` : input);
            setQuotes((list) => [...carried.filter((q) => !list.some((x) => x.conversation === q.conversation && x.reply_id === q.reply_id)), ...list]);
            if (activeConversation.current === conversation) setStatus(String(e).replace(/^Error: /, ""));
        } finally {
            submitting.current.delete(conversation);
            setSending((all) => ({ ...all, [conversation]: false }));
            await loadQueue();
            refresh();
            loadContext();
            loadConversations();
            if (activeConversation.current === conversation) box.current?.focus();
        }
    }

    async function stop() {
        if (stopRequests.current.has(conversation)) return;
        stopRequests.current.add(conversation);
        setStopping((all) => ({ ...all, [conversation]: true }));
        setStatus("");
        try {
            await send(conversation, "/cancel");
            await loadQueue();
            await loadReplies();
        } catch (e) {
            if (activeConversation.current === conversation) setStatus(String(e).replace(/^Error: /, ""));
        } finally {
            stopRequests.current.delete(conversation);
            setStopping((all) => ({ ...all, [conversation]: false }));
            refresh();
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
        if (e.nativeEvent.isComposing) return;
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
    const shownProcess = (selectedReply ? transcript.find((r) => r.id === selectedReply.id) : undefined) ?? (lastWithProcess ? withDelegations(lastWithProcess, delegations) : null);
    const recordedSteps = new Set(entries.flatMap((r) => r.process?.steps?.map((s) => s.id) || []));
    const unrecordedChildren = Object.values(delegations).map(({ step }) => step).filter((s) => !recordedSteps.has(s.id));
    const current = conversations.find((c) => c.id === conversation);
    const title = current?.title || (entries.find((r) => r.kind === "sent")?.input?.split("\n")[0]) || "新会话";
    const listed = conversations.some((c) => c.id === conversation) ? conversations : [{ id: conversation, title: "新会话", last_at: "", count: 0, running: false, project: context?.project?.id, agent: context?.agent?.id }, ...conversations];

    return (
        <div className="flex h-full min-h-0">
            <SessionsTree list={listed} projects={snap.projects} current={conversation} onPick={selectConversation} onNew={(project) => void newSession(project)} creating={creating}
                onUpdate={(id, patch) => void updateConversation(id, patch).then(loadConversations).catch((e) => setStatus(String(e).replace(/^Error: /, "")))}
                collapsed={sessionsCollapsed} onToggle={() => setSessionsCollapsed(!sessionsCollapsed)}
                tasks={snap.tasks} onTask={(t) => { if (t.parent && stepOf(t.id)) { setChild(t); setPickedTask(null); } else setPickedTask(t); }} />
            {pickedTask && <TaskDrawer t={pickedTask} tasks={snap.tasks} plan={snap.plans.find((p) => p.task_id === pickedTask.id)} onClose={() => setPickedTask(null)} width={RAIL_WIDTH} />}

            <div className="flex min-h-0 min-w-0 flex-1 flex-col">
                {hubUpdated && <div role="status" className="border-b border-secondary bg-warning-primary px-6 py-2 text-sm text-secondary">
                    hub 已更新，<button type="button" className="underline" onClick={() => window.location.reload()}>刷新页面</button>
                </div>}
                <header className="flex items-center gap-3 border-b border-secondary bg-primary px-6 py-2.5">
                    <div className="min-w-0 flex-1 truncate text-sm font-semibold text-primary" title={title}>{title}</div>
                    {current?.archived && (
                        <span className="flex items-center gap-1.5">
                            <Badge type="pill-color" size="sm" color="gray">已归档</Badge>
                            <button type="button" className="text-xs text-tertiary hover:text-primary" onClick={() => void updateConversation(current.id, { archived: false }).then(loadConversations).catch((e) => setStatus(String(e).replace(/^Error: /, "")))}>取消归档</button>
                        </span>
                    )}
                    {context?.project && <Badge type="pill-color" size="sm" color="gray">{context.project.id} · {context.project.node}</Badge>}
                    {context?.agent && <Badge type="pill-color" size="sm" color={context.agent.ready ? "brand" : "error"}>{context.agent.id}{context.agent.model ? " · " + context.agent.model : ""}</Badge>}
                    <span role="status" className="text-xs text-tertiary">{status || (creating ? "正在创建会话…" : stopping[conversation] ? "正在停止…" : sending[conversation] ? "正在发送…" : live || busy ? "进行中…" : "")}</span>
                    <span className="ml-1 flex shrink-0 items-center rounded-lg bg-secondary p-0.5 text-xs">
                        <button type="button" onClick={() => navigate("/console")} className={`rounded-md px-2 py-0.5 ${view === "chat" ? "bg-primary text-primary shadow-xs" : "text-tertiary hover:text-primary"}`}>列表</button>
                        <button type="button" onClick={() => navigate("/console?view=board")} className={`rounded-md px-2 py-0.5 ${view === "board" ? "bg-primary text-primary shadow-xs" : "text-tertiary hover:text-primary"}`}>看板</button>
                    </span>
                </header>

                {view === "board" ? <div className="min-h-0 flex-1 overflow-hidden"><BoardPage /></div> : child && stepOf(child.id) ? (
                <div className="min-h-0 flex-1 overflow-y-auto px-8 py-6">
                    <div className="mx-auto flex max-w-3xl flex-col gap-3">
                        <button type="button" onClick={() => setChild(null)} className="self-start text-xs text-tertiary hover:text-primary">← 回到对话</button>
                        {(() => { const st = stepOf(child.id)!; return <DelegationCard key={st.id} id={st.id} info={st} progress={st} open />; })()}
                        <div className="text-xs text-quaternary">这是这次委派的最新过程；任务本身的预算、回合与关系在右栏"关系"里点它可看。</div>
                    </div>
                </div>
                ) : (
                <div className="grid min-h-0 flex-1 grid-cols-1 xl:grid-cols-[minmax(0,1fr)_380px]">
                    <div className="flex min-h-0 flex-col">
                        <div ref={transcriptBox} onScroll={(e) => { const el = e.currentTarget; followTranscript.current = el.scrollHeight - el.clientHeight - el.scrollTop < 48; }} className="min-h-0 flex-1 overflow-y-auto overflow-x-hidden px-8 py-6">
                            {!enabled && <Nothing icon={MessageChatSquare} title="控制台未启用">配置 feishu.owner_open_id：控制台以 owner 身份行事。</Nothing>}
                            {enabled && entries.length === 0 && !live && (
                                <div className="mx-auto max-w-3xl">
                                    <Nothing icon={MessageChatSquare} title="新会话">
                                        {context?.project ? <>会在 <b>{context.project.id}</b>（{context.project.node}）上由 <b>{context.agent?.id || "默认 Agent"}</b> 来做。想换，用输入框下面的两个选择。</> : "说你想做的事。输入 / 看动词，@ 指派一次。"}
                                    </Nothing>
                                </div>
                            )}
                            <div className="mx-auto flex max-w-4xl flex-col gap-5">
                                {transcript.map((r, i) => r.kind === "sent" ? <UserMessage key={r.id || i} text={r.input || ""} /> : <AssistantMessage key={r.id || i} r={r} selected={shownProcess?.id === r.id} onSelect={(r.process || r.injected) ? () => { setSelectedReply(r); setTab("trace"); } : undefined}
                                    onQuote={r.id ? () => setQuotes((list) => list.some((x) => x.reply_id === r.id) ? list : [...list, { conversation, reply_id: r.id!, title: current?.title || conversation, excerpt: (r.text || "").replace(/\s+/g, " ").slice(0, 80) }]) : undefined} />)}
                                {unrecordedChildren.map((s) => <DelegationCard key={s.id} id={s.id} info={s} progress={s} />)}
                                {live && <Working live={live} plans={runningPlans} compact />}
                            </div>
                        </div>
                        <div className="bg-primary px-8 pb-5 pt-2">
                            <Composer
                                value={text} onChange={setText} onSubmit={() => void submit()} onStop={() => void stop()}
                                busy={busy} pending={!!sending[conversation]} stopping={!!stopping[conversation]} disabled={creating || !context} boxRef={box} onKey={onKey}
                                quotes={quotes} onDropQuote={(x) => setQuotes((list) => list.filter((y) => y.reply_id !== x.reply_id))}
                                queue={queue} queueing={queueing} onToggleQueueing={() => setQueueing(!queueing)}
                                onSteer={steer} onDropQueued={(q) => void queueAction(() => deleteQueued(q.id))}
                                onEditQueued={async (q, input) => { try { await editQueued(q.id, input); } finally { await loadQueue(); } }}
                                onSideChat={(q) => void sideChat(q)}
                                suggestions={suggestions} pick={pick} onApply={apply}
                                verbs={verbs} onVerb={(cmd) => { setText(cmd + " "); box.current?.focus(); }}
                                projects={snap.projects} project={context?.project} onProject={(id) => void submit(`/project use ${id}`)}
                                agents={context?.agents ?? []} agent={context?.agent} onAgent={(id) => void submit(`/use ${id}`)}
                                onSelectors={context?.agent ? () => fetchSelectors(conversation, context.agent!.id) : undefined}
                                onPrefer={(patch) => { if (!context?.agent) return; void setPreferences(conversation, context.agent.id, patch).then((r) => { setStatus(r.note || "已记住"); loadContext(); }).catch((e) => setStatus(String(e).replace(/^Error: /, ""))); }}
                            />
                        </div>
                    </div>
                    <Rail context={context} live={live} plans={runningPlans} reply={shownProcess} tab={tab} setTab={setTab} roots={roots} />
                </div>
                )}
            </div>
        </div>
    );
}
