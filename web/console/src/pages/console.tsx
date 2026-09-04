import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router";
import { MessageChatSquare } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Composer } from "@/components/steve/composer";
import { AssistantMessage, UserMessage } from "@/components/steve/message";
import { Rail, type RailTab } from "@/components/steve/rail";
import { SessionsTree } from "@/components/steve/sessions-tree";
import { Working, applyLive, type Live } from "@/components/steve/trace";
import { Nothing } from "@/components/steve/ui";
import { fetchContext, fetchConversations, fetchReplies, fetchSuggest, fetchVerbs, send, updateConversation } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { Conversation, ConversationContext, Reply, Suggestion, Verb } from "@/lib/types";

// ConsolePage is composition: it owns the conversation, the transcript,
// the line in flight and the composer's text, and lays out the three
// columns from components/steve. Nothing here draws.
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
        // The buffer is trimmed from the front, so the cursor is the
        // arrival number of the last event handled, not an index.
        const fresh = consoleEvents.filter((ev) => (ev.n ?? 0) > seen.current);
        if (fresh.length) seen.current = fresh[fresh.length - 1].n ?? seen.current;
        if (fresh.some((ev) => ev.kind === "console.sent" || ev.kind === "console.reply" || ev.kind === "console.meta")) loadConversations();
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
            <SessionsTree list={listed} projects={snap.projects} current={conversation} onPick={(id) => setConversation(id)} onNew={newSession}
                onUpdate={(id, patch) => void updateConversation(id, patch).then(loadConversations).catch((e) => setStatus(String(e).replace(/^Error: /, "")))} />

            <div className="flex min-h-0 min-w-0 flex-1 flex-col">
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
                                {entries.map((r, i) => r.kind === "sent" ? <UserMessage key={i} text={r.input || ""} /> : <AssistantMessage key={i} r={r} selected={shownProcess === r} onSelect={(r.process || r.injected) ? () => { setSelectedReply(r); setTab("trace"); } : undefined} />)}
                                {live && <Working live={live} plans={runningPlans} compact />}
                                <div ref={bottom} />
                            </div>
                        </div>
                        <div className="bg-primary px-8 pb-5 pt-2">
                            <Composer
                                value={text} onChange={setText} onSubmit={() => void submit()} onStop={() => void send(conversation, "/cancel").catch(() => undefined)}
                                busy={busy} boxRef={box} onKey={onKey}
                                suggestions={suggestions} pick={pick} onApply={apply}
                                verbs={verbs} onVerb={(cmd) => { setText(cmd + " "); box.current?.focus(); }}
                                projects={snap.projects} project={context?.project} onProject={(id) => void submit(`/project use ${id}`)}
                                agents={context?.agents ?? []} agent={context?.agent} onAgent={(id) => void submit(`/use ${id}`)}
                            />
                        </div>
                    </div>
                    <Rail context={context} live={live} plans={runningPlans} reply={shownProcess} tab={tab} setTab={setTab} roots={roots} />
                </div>
            </div>
        </div>
    );
}
