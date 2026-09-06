import { useCallback, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore, type SetStateAction } from "react";
import { useLocation, useNavigate } from "react-router";
import { LayoutLeft, LayoutRight, MessageChatSquare, X } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Composer, type Queued } from "@/components/steve/composer";
import { AssistantMessage, UserMessage } from "@/components/steve/message";
import { Rail, type RailTab } from "@/components/steve/rail";
import { ResizableInspector } from "@/components/steve/resizable-inspector";
import { useReviewActive } from "@/components/steve/review-context";
import { SessionsTree } from "@/components/steve/sessions-tree";
import { TaskDrawer } from "@/components/steve/task-drawer";
import { DelegationCard } from "@/components/steve/delegation";
import { RAIL_WIDTH } from "@/components/steve/rail";
import { BoardPage } from "./board";
import { Working } from "@/components/steve/trace";
import { applyLive, type Live } from "@/lib/live";
import { Nothing } from "@/components/steve/ui";
import { enqueue, fetchQueue, deleteQueued, editQueued, steerQueued, fetchContext, fetchConversations, fetchReplies, fetchSuggest, fetchVerbs, send, updateConversation, fetchSelectors, setPreferences, checkSubmissionSupport, requireSubmissionSupport, getSubmissionSupport, subscribeSubmissionSupport } from "@/lib/api/console";
import { Sheet } from "@/components/steve/drawer";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { useResourceRead } from "@/hooks/use-resource-read";
import { placeLabel } from "@/lib/workspaces";
import { useFleet, useIntent } from "@/lib/fleet";
import { applyDelegation, restoreDelegations, withDelegations, type Delegations } from "@/lib/delegations";
import { beginSubmission, retrySubmission, failSubmission, finishSubmission, reconcileSubmission, restoreSubmission, updateDraft, useDraft, useQuotes, useSubmission, useStops, beginStop, finishStop, isStopPending, clearStopNotice, type Submission, useMaterials, removeDraftMaterial, submissionRefs } from "@/lib/drafts";
import { useI18n } from "@/providers/locale-provider";
import { useMaterial } from "@/providers/material-provider";
import { uploadMaterial, refKey } from "@/lib/api/material";
import { MaterialPreview } from "@/components/steve/material-shelf";
import { QuestionPanel } from "@/components/steve/question-panel";
import type { MaterialRef } from "@/lib/types";
import { HTTPError, isRejectedRequest } from "@/lib/http";
import type { Conversation, ConversationContext, Reply, Suggestion, Verb, Task, StepProcess, Exchange } from "@/lib/types";

// ConsolePage is composition: it owns the conversation, the transcript,
// the line in flight and the composer's text, and lays out the three
// columns from components/steve. Nothing here draws.
export function ConsolePage() {
    const { snap, consoleEvents, refresh, live: connection, hubUpdated } = useFleet();
    const { t, locale } = useI18n();
    const materials = useMaterial();
    const [uploading, setUploading] = useState(false);
    const [openedMaterial, setOpenedMaterial] = useState<MaterialRef | null>(null);
    const { intent, consume } = useIntent();
    const [conversation, setConversation] = useState(() => sessionStorage.getItem("steve.conversation") || "console:main");
    const [conversations, setConversations] = useState<Conversation[]>([]);
    const [entries, setEntries] = useState<Reply[]>([]);
    const [loadingReplies, setLoadingReplies] = useState(true);
    const [enabled, setEnabled] = useState(true);
    const text = useDraft(conversation);
    const draftMaterials = useMaterials(conversation);
    function writeDraft(id: string, value: SetStateAction<string>) {
        if (!updateDraft(id, value)) setStatus(t("console.draftStorage"));
    }
    const setText = (value: SetStateAction<string>) => writeDraft(conversation, value);
    const submission = useSubmission(conversation);
    const submissionSupport = useSyncExternalStore(subscribeSubmissionSupport, getSubmissionSupport);
    const canSubmit = submissionSupport.state === "supported";
    const stops = useStops();
    const stopState = stops[conversation];
    const stopping = !!stopState?.active;
    const [creating, setCreating] = useState(false);
    const creatingRequest = useRef(false);
    const [status, setStatus] = useState("");
    const [contextError, setContextError] = useState("");
    const [conversationsError, setConversationsError] = useState("");
    const [live, setLive] = useState<Live | null>(null);
    const [delegations, setDelegations] = useState<Delegations>({});
    const transcript = entries.map((r) => withDelegations(r, delegations));
    const [context, setContext] = useState<ConversationContext | null>(null);
    const [suggestions, setSuggestions] = useState<Suggestion[]>([]);
    const [pick, setPick] = useState(0);
    const [selectedReply, setSelectedReply] = useState<Reply | null>(null);
    const [tab, setTab] = useState<RailTab>("context");
    const desktopSessions = useBreakpoint("lg");
    const canDockInspector = useBreakpoint("2xl");
    const canResizeInspector = useBreakpoint("sm");
    const reviewing = useReviewActive();
    const inspectorLayout = useRef({ dock: canDockInspector, resize: canResizeInspector });
    // Opening a new modal behind Review would steal its focus/top layer.
    if (!reviewing) inspectorLayout.current = { dock: canDockInspector, resize: canResizeInspector };
    const { dock: dockInspector, resize: resizeInspector } = inspectorLayout.current;
    const [mobileSessions, setMobileSessions] = useState(false);
    const [inspectorOpen, setInspectorOpen] = useState(false);
    useEffect(() => { if (!reviewing && materials.sideRequest && context?.project && materials.sideRequest.project === context.project.id) { setInspectorOpen(true); setTab("materials"); } }, [materials.sideRequest, reviewing, context?.project?.id]);
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
    const transcriptRevision = useRef(0);
    // Quotes ride with the next message wherever it is sent from; they
    // survive switching threads on purpose — that is how a line from one
    // thread reaches another.
    const [quotes, setQuotes] = useQuotes();
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

    const loadContext = useResourceRead(`context:${conversation}`, (signal) => fetchContext(conversation, signal),
        (data) => { setContext(data.context ?? null); setContextError(""); }, (error) => setContextError(String(error).replace(/^Error: /, "")));
    const loadConversations = useResourceRead("conversations", fetchConversations,
        (data) => { setConversations(data.conversations || []); setConversationsError(""); }, (error) => setConversationsError(String(error).replace(/^Error: /, "")));

    const loadQueue = useCallback(async () => {
        if (activeConversation.current !== conversation) return;
        const request = ++queueRequest.current;
        try {
            const data = await fetchQueue(conversation);
            if (activeConversation.current !== conversation || request !== queueRequest.current) return;
            setExchanges(data.queue || []);
            reconcileSubmission(conversation, data.queue || []);
            const running = [...(data.queue || [])].filter((e) => e.state === "running").sort((a, b) => (b.started_at || "").localeCompare(a.started_at || ""))[0];
            setLive((cur) => running ? (cur?.exchangeID === running.id ? cur : { since: running.started_at || running.enqueued_at, exchangeID: running.id, steps: {}, order: [] }) : null);
        } catch (e) {
            if (activeConversation.current === conversation) setStatus(String(e).replace(/^Error: /, ""));
        }
    }, [conversation]);
    const loadReplies = useCallback(async () => {
        if (activeConversation.current !== conversation) return;
        const request = ++transcriptRequest.current;
        try {
            // An SSE mutation may be newer than an in-flight HTTP snapshot.
            // Retry once; continuous traffic settles through the regular poll.
            for (let attempt = 0; attempt < 2; attempt++) {
                const revision = transcriptRevision.current;
                const cursor = seen.current;
                const data = await fetchReplies(conversation);
                if (activeConversation.current !== conversation || request !== transcriptRequest.current) return;
                if (revision !== transcriptRevision.current) continue;
                setEnabled(data.enabled);
                setEntries(data.replies || []);
                setLoadingReplies(false);
                setDelegations((cur) => restoreDelegations(cur, data.replies || [], cursor));
                return;
            }
        } catch (e) {
            if (activeConversation.current === conversation) { setStatus(String(e)); setLoadingReplies(false); }
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

    // Completion can arrive in an old route instance; the current subscriber
    // refreshes its own projection when the shared stop operation settles.
    useEffect(() => {
        if (stopState && !stopState.active) { void loadQueue(); void loadReplies(); }
    }, [stopState, loadQueue, loadReplies]);

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
        const lines = mine.filter((ev) => ["console.sent", "console.reply", "console.notice", "console.milestone", "console.recalled"].includes(ev.kind));
        if (lines.length) transcriptRevision.current++;
        setLive((cur) => mine.reduce(applyLive, cur));
        setDelegations((cur) => mine.reduce(applyDelegation, cur));
        if (mine.some((ev) => ev.kind === "console.queue")) void loadQueue();
        if (mine.some((ev) => ev.kind === "console.sent" || ev.kind === "console.reply")) void loadReplies();
        if (mine.some((ev) => ev.kind === "console.reply")) { loadContext(); refresh(); }
        if (!lines.length) return;
        setEntries((list) => {
            const next = [...list];
            for (const ev of lines) {
                if (ev.kind === "console.recalled") {
                    const index = next.findIndex((r) => ev.reply_id && r.id === ev.reply_id);
                    if (index >= 0) next.splice(index, 1);
                    continue;
                }
                const kind = ev.kind.slice("console.".length);
                const r: Reply = { id: ev.reply_id, exchange_id: ev.exchange_id, at: ev.at, conversation, kind, title: ev.title, text: kind === "sent" ? "" : ev.text || "", format: ev.format, input: kind === "sent" ? ev.text : undefined };
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
        if (intent.mode === "run" && (!canSubmit || !context || submission || creating || isStopPending(conversation))) return;
        handled.current = intent.n;
        consume(intent.n);
        if (intent.mode === "fill") { setText(intent.text + " "); box.current?.focus(); }
        else void submit(intent.text);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [intent, context, submission, creating, stopping, canSubmit, consume]);

    // The composer grows with the text, up to a few lines, like a chat app's.
    useEffect(() => {
        const el = box.current;
        if (!el) return;
        el.style.height = "0px";
        el.style.height = Math.min(el.scrollHeight, 200) + "px";
    }, [text]);

    function selectConversation(id: string, nextContext: ConversationContext | null = null) {
        setMobileSessions(false);
        if (id === activeConversation.current) return;
        activeConversation.current = id;
        followTranscript.current = true;
        setConversation(id);
        setEntries([]);
        setLoadingReplies(true);
        setExchanges([]);
        setContext(nextContext);
        setContextError("");
        setLive(null);
        setChild(null);
        setPickedTask(null);
        setSelectedReply(null);
        setStatus("");
        window.setTimeout(() => box.current?.focus(), 0);
    }

    async function newSession(project = context?.project?.id) {
        if (creatingRequest.current) return;
        setMobileSessions(false);
        creatingRequest.current = true;
        setCreating(true);
        setStatus("");
        const from = conversation;
        const id = `console:${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
        try {
            if (!project) throw new Error(t("console.projectNotReady"));
            await send(id, `/project use ${project}`);
            const data = await fetchContext(id);
            if (data.context?.project?.id !== project || !data.context.project.bound) throw new Error(t("console.bindingFailed"));
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
            await requireSubmissionSupport();
            // Finish binding before submitting: a timer can race a slow hub.
            if (context?.project?.id) await send(id, `/project use ${context.project.id}`);
            await deleteQueued(q.id);
            try { await enqueue(id, q.input, q.quotes); }
            catch (e) { writeDraft(id, q.input); setQuotes(q.quotes || []); selectConversation(id); throw e; }
            selectConversation(id);
        });
    }

    async function deliver(pending: Submission) {
        try {
            await enqueue(conversation, pending.input, pending.quotes.length ? pending.quotes : undefined, pending.id, submissionRefs(pending), pending.locale);
            finishSubmission(conversation, pending.id);
            if (activeConversation.current === conversation) setSelectedReply(null);
        } catch (error) {
            const conflict = error instanceof HTTPError && error.status === 409;
            const message = error instanceof Error ? error.message : String(error);
            const retained = failSubmission(conversation, pending.id, message, conflict ? "conflict" : isRejectedRequest(error) ? "rejected" : "unknown");
            if (retained && activeConversation.current === conversation) setStatus(isRejectedRequest(error) && !pending.uncertain ? message : "");
        } finally {
            await loadQueue();
            refresh(); loadContext(); loadConversations();
            if (activeConversation.current === conversation) box.current?.focus();
        }
    }

    async function submit(line?: string) {
        const input = (line ?? text).trim();
        if ((!input && !draftMaterials.length) || uploading || !canSubmit || submission || isStopPending(conversation) || creatingRequest.current || !context) return;
        if (busy && !queueing) { setStatus(t("console.queueDisabled")); return; }
        let pending: Submission | null;
        try { pending = beginSubmission(conversation, input, quotes, line === undefined, locale); }
        catch { setStatus(t("console.pendingStorage")); return; }
        if (!pending) return;
        setStatus(""); clearStopNotice(conversation);
        followTranscript.current = true;
        await deliver(pending);
    }

    async function retrySend() {
        if (!canSubmit || isStopPending(conversation)) return;
        let pending: Submission | null;
        try { pending = retrySubmission(conversation); }
        catch { setStatus(t("console.retryStorage")); return; }
        if (pending) { setStatus(""); await deliver(pending); }
    }

    async function stop() {
        const id = beginStop(conversation);
        if (!id) return;
        setStatus("");
        try {
            const reply = await send(conversation, "/cancel", undefined, id);
            finishStop(conversation, id, { message: reply.text || t("console.stopDone") });
        } catch (error) {
            finishStop(conversation, id, { error: error instanceof Error ? error.message : String(error), uncertain: !isRejectedRequest(error) });
        } finally { refresh(); }
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
    const title = current?.title || (entries.find((r) => r.kind === "sent")?.input?.split("\n")[0]) || t("console.newConversation");
    useEffect(() => { materials.setTarget(context?.project ? { conversation, project: context.project.id, title } : null); }, [conversation, context?.project?.id, title, materials.setTarget]);
    async function upload(files: FileList | null) {
        if (!files?.length || uploading || !context?.project || !submissionSupport.material_refs) return;
        const target = { conversation, project: context.project.id, title };
        setUploading(true); setStatus("");
        try { for (const file of Array.from(files)) { const material = await uploadMaterial(target.project, file, locale); materials.add(material, { id: material.id }, target); } }
        catch (error) { setStatus(error instanceof Error ? error.message : String(error)); }
        finally { setUploading(false); }
    }
    const listed = conversations.some((c) => c.id === conversation) ? conversations : [{ id: conversation, title: t("console.newConversation"), last_at: "", count: 0, running: false, project: context?.project?.id, agent: context?.agent?.id }, ...conversations];

    const sessions = (collapsed = sessionsCollapsed) => <SessionsTree list={listed} projects={snap.projects} current={conversation} onPick={selectConversation} onNew={(project) => void newSession(project)} creating={creating}
                onUpdate={(id, patch) => void updateConversation(id, patch).then(loadConversations).catch((e) => setStatus(String(e).replace(/^Error: /, "")))}
                collapsed={collapsed} onToggle={() => desktopSessions ? setSessionsCollapsed(!sessionsCollapsed) : setMobileSessions(false)}
                tasks={snap.tasks} onTask={(t) => { setMobileSessions(false); if (t.parent && stepOf(t.id)) { setChild(t); setPickedTask(null); } else setPickedTask(t); }} />;
    const inspector = <Rail key={conversation} context={context} live={live} plans={runningPlans} reply={shownProcess} tab={tab} setTab={setTab} roots={roots} onClose={() => setInspectorOpen(false)} />;
    return (
        <div className="console-workbench">
            {view === "chat" && desktopSessions && sessions()}
            {mobileSessions && !desktopSessions && <Sheet label={t("console.sessions")}  side="left" width={300} onClose={() => setMobileSessions(false)}><button type="button" className="sheet-close workbench-icon-button" aria-label={t("console.closeSessions")}  onClick={() => setMobileSessions(false)}><X aria-hidden="true" /></button>{sessions(false)}</Sheet>}
            {pickedTask && <TaskDrawer t={pickedTask} tasks={snap.tasks} plan={snap.plans.find((p) => p.task_id === pickedTask.id)} onClose={() => setPickedTask(null)} width={RAIL_WIDTH} />}

            <div className="console-main">
                {hubUpdated && <div role="status" className="border-b border-secondary bg-warning-primary px-6 py-2 text-sm text-secondary">
                    {t("console.hubUpdated")}<button type="button" className="underline" onClick={() => { if (window.confirm(t("console.reloadConfirm"))) window.location.reload(); }}>{t("console.reloadPage")}</button>
                </div>}
                {view === "chat" && <header className="console-toolbar">
                    <button type="button" className="workbench-icon-button" aria-label={t("console.sessions")}  title={t("console.sessions")}  onClick={() => desktopSessions ? setSessionsCollapsed(!sessionsCollapsed) : setMobileSessions(true)}><LayoutLeft aria-hidden="true" /></button>
                    <div className="console-heading">
                    <h1 title={title}>{title}</h1>
                    {context?.project && <div className="console-location" title={context.agent?.place ? placeLabel(context.agent.place, locale) : context.project.path}>{context.project.id} · {context.agent?.place ? placeLabel(context.agent.place, locale) : context.project.node}</div>}
                    </div>
                    {current?.archived && (
                        <span className="flex items-center gap-1.5">
                            <Badge type="pill-color" size="sm" color="gray">{t("console.archived")}</Badge>
                            <button type="button" className="text-xs text-tertiary hover:text-primary" onClick={() => void updateConversation(current.id, { archived: false }).then(loadConversations).catch((e) => setStatus(String(e).replace(/^Error: /, "")))}>{t("console.unarchive")}</button>
                        </span>
                    )}
                    <span role="status" className="console-status">{status || contextError || conversationsError || stopState?.error || stopState?.message || (creating ? t("console.creating") : stopping ? t("console.stopping") : submission?.active ? t("console.sending") : live || busy ? t("console.working") : "")}</span>
                    <span className="workbench-segmented" role="group" aria-label={t("console.workView")} >
                        <button type="button" onClick={() => navigate("/console")} aria-pressed>{t("console.conversation")}</button>
                        <button type="button" onClick={() => navigate("/console?view=board")} aria-pressed={false}>{t("console.board")}</button>
                    </span>
                    {view === "chat" && <button type="button" className="workbench-icon-button inspector-toggle" aria-label={inspectorOpen ? t("console.hideDetails") : t("console.showDetails")} aria-pressed={inspectorOpen} title={inspectorOpen ? t("console.hideDetails") : t("console.showDetails")} onClick={() => setInspectorOpen(!inspectorOpen)}><LayoutRight aria-hidden="true" /></button>}
                </header>}

                {view === "board" ? <div className="min-h-0 flex-1 overflow-hidden"><BoardPage /></div> : child && stepOf(child.id) ? (
                <div className="min-h-0 flex-1 overflow-y-auto px-8 py-6">
                    <div className="mx-auto flex max-w-3xl flex-col gap-3">
                        <button type="button" onClick={() => setChild(null)} className="self-start text-xs text-tertiary hover:text-primary">{t("console.backConversation")}</button>
                        {(() => { const st = stepOf(child.id)!; return <DelegationCard key={st.id} id={st.id} info={st} progress={st} open />; })()}
                        <div className="text-xs text-quaternary">{t("console.delegationHint")}</div>
                    </div>
                </div>
                ) : (
                <div className={`console-content ${inspectorOpen && dockInspector ? "with-inspector" : ""}`}>
                    <div className="conversation-content">
                        <div ref={transcriptBox} onScroll={(e) => { const el = e.currentTarget; followTranscript.current = el.scrollHeight - el.clientHeight - el.scrollTop < 48; }} className="transcript-scroll min-h-0 flex-1 overflow-y-auto overflow-x-hidden">
                            {!enabled && <Nothing icon={MessageChatSquare} title={t("console.disabled")} >{t("console.disabledHint")}</Nothing>}
                            {enabled && entries.length === 0 && !live && (
                                <div className="mx-auto max-w-3xl">
                                    <Nothing icon={MessageChatSquare} title={loadingReplies ? t("console.loadingConversation") : t("console.newConversation")}>
                                        {loadingReplies ? t("console.loadingReplies") : context?.project ? t("console.startInProject", { project: context.project.id }) : t("console.emptyHint")}
                                    </Nothing>
                                </div>
                            )}
                            <div className="transcript-messages">
                                {transcript.map((r, i) => r.kind === "sent" ? <UserMessage key={r.id || i} text={r.input || ""} /> : <AssistantMessage key={r.id || i} r={r} selected={shownProcess?.id === r.id} onSelect={(r.process || r.injected) ? () => { setSelectedReply(r); setTab("trace"); setInspectorOpen(true); } : undefined}
                                    onQuote={r.id ? () => setQuotes((list) => list.some((x) => x.reply_id === r.id) ? list : [...list, { conversation, reply_id: r.id!, title: current?.title || conversation, excerpt: (r.text || "").replace(/\s+/g, " ").slice(0, 80) }]) : undefined} />)}
                                {unrecordedChildren.map((s) => <DelegationCard key={s.id} id={s.id} info={s} progress={s} />)}
                                {live && <Working live={live} plans={runningPlans} compact />}
                            </div>
                        </div>
                        <div className="composer-dock">
                            {!canSubmit && <div role="status" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary">
                                <p>{submissionSupport.state === "unsupported" ? t("console.unsupportedHub") : submissionSupport.error ? t("console.unknownHub") : t("console.checkingHub")}</p>
                                {submissionSupport.error && <p className="mt-1 break-words">{submissionSupport.error}</p>}
                                <button type="button" className="mt-1 underline disabled:opacity-50" disabled={submissionSupport.checking} onClick={() => void checkSubmissionSupport()}>{submissionSupport.checking ? t("console.checking") : t("console.checkAgain")}</button>
                            </div>}
                            {submission && !submission.active && <div role="alert" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary">
                                <p>{submission.conflict ? t("console.submissionConflict") : submission.rejected ? t("console.restoreFailed") : t("console.uncertain")}</p>
                                {submission.error && <p className="mt-1 break-words">{submission.error}</p>}
                                <p className="my-1 whitespace-pre-wrap break-words">{submission.input}</p>
                                {submission.id && !submission.conflict && !submission.rejected && <button type="button" className="mr-4 underline" onClick={() => void retrySend()}>{t("console.retrySend")}</button>}
                                {(!submission.id || submission.rejected) && <button type="button" className="mr-4 underline" onClick={() => { if (!restoreSubmission(conversation)) setStatus(t("console.recoveryStorage")); }}>{t("console.restoreDraft")}</button>}
                                <button type="button" className="underline" onClick={() => { finishSubmission(conversation, submission.id); setStatus(""); }}>{t("console.received")}</button>
                            </div>}
                            {stopState?.uncertain && <div role="alert" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary">
                                <p>{t("console.stopUncertain")}</p>
                                <button type="button" className="mt-1 underline" onClick={() => void stop()}>{t("console.retryStop")}</button>
                            </div>}
                            {submissionSupport.interactive_requests && <QuestionPanel key={conversation} conversation={conversation} />}
                            {draftMaterials.length > 0 && <ul aria-label={t("materials.draftRefs")} className="mx-auto mb-2 flex max-w-3xl flex-wrap gap-2">{draftMaterials.map((ref) => <li key={refKey(ref)} className="flex max-w-full items-center gap-2 rounded-lg bg-secondary px-3 py-2 text-xs"><button type="button" className="truncate" onClick={() => setOpenedMaterial(ref)}>{ref.title}{ref.selector?.kind === "lines" ? ` · L${ref.selector.start}–L${ref.selector.end}` : ""}</button><button type="button" aria-label={t("materials.remove", { title: ref.title })} onClick={() => { if (!removeDraftMaterial(conversation, ref)) setStatus(t("materials.sourceUnavailable")); }}>×</button></li>)}</ul>}
                            {submissionSupport.material_refs && <div className="mx-auto mb-2 flex max-w-3xl flex-wrap items-center gap-3"><label className="cursor-pointer rounded-md px-2 py-1 text-xs text-tertiary hover:bg-secondary">{uploading ? t("materials.uploading") : t("materials.upload")}<input type="file" multiple className="sr-only" disabled={uploading} aria-label={t("materials.upload")} onChange={(event) => { void upload(event.target.files); event.target.value = ""; }} /></label><span className="text-xs text-quaternary">{t("materials.uploadHint")}</span></div>}
                            {openedMaterial && context?.project && <MaterialPreview project={context.project.id} anchor={openedMaterial} onClose={() => setOpenedMaterial(null)} />}
                            <Composer
                                value={text} hasMaterials={draftMaterials.length > 0} onChange={setText} onSubmit={() => void submit()} onStop={() => void stop()}
                                busy={busy} pending={!!submission} stopping={stopping} disabled={creating || !context || !canSubmit} boxRef={box} onKey={onKey}
                                quotes={quotes} onDropQuote={(x) => setQuotes((list) => list.filter((y) => y.reply_id !== x.reply_id))}
                                queue={queue} queueing={queueing} onToggleQueueing={() => setQueueing(!queueing)}
                                onSteer={steer} onDropQueued={(q) => void queueAction(() => deleteQueued(q.id))}
                                onEditQueued={async (q, input) => { try { await editQueued(q.id, input); } finally { await loadQueue(); } }}
                                onSideChat={(q) => void sideChat(q)}
                                suggestions={suggestions} pick={pick} onApply={apply}
                                verbs={verbs} onVerb={(cmd) => { setText(cmd + " "); box.current?.focus(); }}
                                projects={snap.projects} project={context?.project} onProject={(id) => void submit(`/project use ${id}`)}
                                agents={context?.agents ?? []} agent={context?.agent} onAgent={(id) => void submit(`/use ${id}`)}
                                preferenceKey={`${conversation}:${context?.agent?.id || ""}`}
                                onSelectors={context?.agent ? () => fetchSelectors(conversation, context.agent!.id) : undefined}
                                onPrefer={async (patch) => { if (!context?.agent) return; const r = await setPreferences(conversation, context.agent.id, patch); if (activeConversation.current === conversation) { setStatus(r.note || t("console.preferenceSaved")); loadContext(); } }}
                            />
                        </div>
                    </div>
                    {inspectorOpen && dockInspector && <ResizableInspector>{inspector}</ResizableInspector>}
                {inspectorOpen && !dockInspector && <Sheet label={t("console.details")}  width={resizeInspector ? "max-content" : 360} onClose={() => setInspectorOpen(false)}>{resizeInspector ? <ResizableInspector overlay>{inspector}</ResizableInspector> : inspector}</Sheet>}
                </div>
                )}
            </div>
        </div>
    );
}
