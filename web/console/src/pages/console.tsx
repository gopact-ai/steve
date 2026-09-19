import { useSideChat } from "@/providers/side-chat-provider";
import { SideChatPanel } from "@/components/steve/side-chat";
import { useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore, type SetStateAction } from "react";
import { useLocation, useNavigate } from "react-router";
import { Columns03, LayoutLeft, LayoutRight, MessageChatSquare, X } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { ThemeMenu } from "@/components/steve/theme-menu";
import type { DraftBox } from "@/components/steve/markdown-input";
import { Composer, type Queued } from "@/components/steve/composer";
import { AssistantMessage, UserMessage } from "@/components/steve/message";
import { Rail, type RailTab } from "@/components/steve/rail";
import { ResizableInspector } from "@/components/steve/resizable-inspector";
import { useReviewActive } from "@/components/steve/review-context";
import { closeOrder, useCloseLayer } from "@/providers/close-stack";
import { NativeSessionImport } from "@/components/steve/native-session-import";
import { SessionsTree } from "@/components/steve/sessions-tree";
import { TaskDrawer } from "@/components/steve/task-drawer";
import { DelegationCard, DelegationPanel } from "@/components/steve/delegation";
import { SplitPane, SplitPaneProvider, useSplitPane, type SplitTab } from "@/components/steve/split-pane";
import { RAIL_WIDTH } from "@/components/steve/rail";
import { BoardPage } from "./board";
import { Working } from "@/components/steve/trace";
import { conversationContextRevision } from "@/lib/conversation-context-revision";
import { Nothing } from "@/components/steve/ui";
import { enqueue, deleteConversation, deleteQueued, editQueued, steerQueued, fetchContext, fetchConversations, fetchSuggest, fetchVerbs, send, updateConversation, initializeConversation, fetchSelectors, setPreferences, checkSubmissionSupport, requireSubmissionSupport, getSubmissionSupport, subscribeSubmissionSupport } from "@/lib/api/console";
import { Sheet } from "@/components/steve/drawer";
import { ConfirmDialog } from "@/components/steve/confirm";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { useEventCallback } from "@/hooks/use-event-callback";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useConversationController } from "@/hooks/use-conversation-controller";
import { placeLabel } from "@/lib/workspaces";
import { useConsoleEvents, useFleet, useIntent } from "@/lib/fleet";
import { childrenOfTurn, streamWithChildren, withDelegations } from "@/lib/delegations";
import { beginSubmission, retrySubmission, failSubmission, finishSubmission, restoreSubmission, updateDraft, useDraft, useDraftIssue, useSavedDraft, resolveDraftConflict, useQuotes, useSubmission, useStops, beginStop, finishStop, isStopPending, clearStopNotice, type Submission, useMaterials, removeDraftMaterial, submissionRefs, useRewind, beginRewind, endRewind, rewindOf } from "@/lib/drafts";
import { useI18n } from "@/providers/locale-provider";
import { useMaterial } from "@/providers/material-provider";
import { uploadMaterial, refKey } from "@/lib/api/material";
import { MaterialPreview } from "@/components/steve/material-shelf";
import { QuestionPanel } from "@/components/steve/question-panel";
import type { MaterialRef } from "@/lib/types";
import { HTTPError, isRejectedRequest } from "@/lib/http";
import { useNodeLabel } from "@/lib/node-name";
import type { Conversation, ConversationContext, Reply, Suggestion, Verb, Task, StepProcess } from "@/lib/types";

// ConsolePage composes the conversation controller, draft commands and the
// three workbench columns. Public transcript/live state is shared with the
// side conversation; layout and user actions stay local to this surface.
// Browsers hand over every pasted screenshot as "image.png", so a stamped
// name keeps one paste apart from the next in the draft row.
function stamped(file: File): File {
    if (file.name && !/^(image|clipboard|screenshot)\.[a-z\d]+$/i.test(file.name)) return file;
    const ext = (file.name.split(".").pop() || file.type.split("/")[1] || "png").toLowerCase().replace(/\+xml$/, "");
    const at = new Date(), pad = (n: number) => String(n).padStart(2, "0");
    return new File([file], `pasted-${at.getFullYear()}${pad(at.getMonth() + 1)}${pad(at.getDate())}-${pad(at.getHours())}${pad(at.getMinutes())}${pad(at.getSeconds())}.${ext === "jpeg" ? "jpg" : ext}`, { type: file.type });
}

// The pane beside the conversation is opened from inside the transcript
// — a delegated child's card, a passage asked about — so the page it
// belongs to is what carries it.
export function ConsolePage() {
    return <SplitPaneProvider><ConsoleWorkbench /></SplitPaneProvider>;
}

function ConsoleWorkbench() {
    const { snap, refresh, live: connection, hubUpdated } = useFleet();
    const nodeLabelOf = useNodeLabel();
    const { t, locale } = useI18n();
    const materials = useMaterial();
    const side = useSideChat();
    const [uploading, setUploading] = useState(false);
    const [openedMaterial, setOpenedMaterial] = useState<MaterialRef | null>(null);
    const { intent, consume } = useIntent();
    const [conversation, setConversation] = useState(() => sessionStorage.getItem("steve.conversation") || "console:main");
    const [conversations, setConversations] = useState<Conversation[]>([]);
    const { entries, loadingReplies, enabled, live, delegations, exchanges, busy, queueError, replyError, loadQueue, invalidateReplies } = useConversationController(conversation);
    const text = useDraft(conversation);
    const rewind = useRewind(conversation);
    const draftIssue = useDraftIssue(conversation);
    const savedDraft = useSavedDraft(conversation);
    const draftMaterials = useMaterials(conversation);
    function writeDraft(id: string, value: SetStateAction<string>) { void updateDraft(id, value); }
    const setText = (value: SetStateAction<string>) => writeDraft(conversation, value);
    const submission = useSubmission(conversation);
    const submissionSupport = useSyncExternalStore(subscribeSubmissionSupport, getSubmissionSupport);
    const canSubmit = submissionSupport.state === "supported";
    const stops = useStops();
    const stopState = stops[conversation];
    const stopping = !!stopState?.active;
    const [creating, setCreating] = useState(false);
    const [importing, setImporting] = useState(false);
    const creatingRequest = useRef(false);
    const [status, setStatus] = useState("");
    const readErrorText = (error: unknown) => error instanceof TypeError && /^(?:Load failed|Failed to fetch|NetworkError.*)$/i.test(error.message)
        ? t("connection.partial") : String(error).replace(/^(?:Error|TypeError): /, "");
    const [contextError, setContextError] = useState("");
    const [conversationsError, setConversationsError] = useState("");
    // Stopping a turn is an act on that turn: the control and its receipt
    // stay in the ledger, the transcript stays what was said and answered.
    const transcript = entries.filter((r) => !r.silent).map((r) => withDelegations(r, delegations));
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
    const split = useSplitPane()!;
    const { open: openSplit, close: closeSplit, closeKind: closeSplitKind } = split;
    // A side chat is a tab in the pane like any other: opening one from a
    // passage opens the pane, and closing its tab ends the side chat.
    const sideChatID = side.session?.id;
    useEffect(() => { if (sideChatID) openSplit({ id: "chat", kind: "chat" }); else closeSplit("chat"); }, [sideChatID, openSplit, closeSplit]);
    useEffect(() => { if (!reviewing && materials.sideRequest && context?.project && materials.sideRequest.project === context.project.id) { setInspectorOpen(true); setTab("materials"); } }, [materials.sideRequest, reviewing, context?.project?.id]);
    // A line already sent cannot be unsaid, but the thread can go back to
    // just before it: editing puts that line in the box, and sending it
    // removes everything that followed. Text already typed there is not
    // thrown away without asking, and is given back if the edit is
    // cancelled.
    const [replacingDraft, setReplacingDraft] = useState<{ reply: string; text: string } | null>(null);
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
    useEffect(() => { closeSplitKind("delegation"); }, [conversation, closeSplitKind]);
    // The server owns execution; every tab projects the same durable queue.
    const queue = useMemo(() => exchanges.filter((e) => e.conversation === conversation && e.state === "queued"), [exchanges, conversation]);
    const recoveryState = exchanges.find((entry) => entry.conversation === conversation && ["recovering", "awaiting-user"].includes(entry.state))?.state;
    const activeConversation = useRef(conversation);
    activeConversation.current = conversation;
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
    const box = useRef<DraftBox | null>(null);
    const transcriptBox = useRef<HTMLDivElement>(null);
    const followTranscript = useRef(true);
    const handled = useRef(0);

    useEffect(() => { sessionStorage.setItem("steve.conversation", conversation); }, [conversation]);

    // "/console?new=1&project=x&prompt=y" — from the projects or profile
    // page: a fresh thread bound to that project, carrying a draft the
    // reader can still edit before sending. The address is cleaned up.
    const location = useLocation();
    const view = new URLSearchParams(location.search).get("view") === "board" ? "board" : "chat";
    const navigate = useNavigate();
    const opened = useRef(false);
    useEffect(() => {
        const params = new URLSearchParams(location.search);
        if (!params.get("new") || opened.current) return;
        opened.current = true;
        const project = params.get("project") || undefined;
        const prompt = params.get("prompt") || "";
        navigate("/console", { replace: true });
        void newSession(project, prompt);
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
        (data) => { setContext(data.context ?? null); setContextError(""); }, (error) => setContextError(readErrorText(error)));
    const loadConversations = useResourceRead("conversations", fetchConversations,
        (data) => { setConversations(data.conversations || []); setConversationsError(""); }, (error) => setConversationsError(readErrorText(error)));

    useEffect(() => {
        loadContext();
        loadConversations();
        setSelectedReply(null);
        setChild(null);
    }, [conversation, loadContext, loadConversations]);

    useEffect(() => {
        if (connection === "live") { loadConversations(); loadContext(); }
        // Recover conversation binding/agent changes whose meta event was
        // missed, without coupling this resource to fleet observation time.
        const timer = window.setInterval(() => { loadConversations(); loadContext(); }, 10000);
        return () => window.clearInterval(timer);
    }, [connection, loadConversations, loadContext]);

    const contextRevision = useMemo(() => conversationContextRevision(snap), [snap.agents, snap.projects]);
    useEffect(() => { loadContext(); }, [contextRevision, loadContext]);

    // Global list and fleet effects belong to this workbench, not to the
    // conversation projection shared with the independently mounted side chat.
    useConsoleEvents((fresh) => {
        if (fresh.some((ev) => ["console.sent", "console.reply", "console.meta", "console.queue", "console.question"].includes(ev.kind))) loadConversations();
        const mine = fresh.filter((ev) => ev.conversation === conversation);
        if (mine.some((ev) => ev.kind === "console.reply" || ev.kind === "console.meta")) loadContext();
        if (mine.some((ev) => ev.kind === "console.reply")) refresh();
    });

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

    function selectConversation(id: string, nextContext: ConversationContext | null = null) {
        setMobileSessions(false);
        if (id === activeConversation.current) return;
        activeConversation.current = id;
        followTranscript.current = true;
        setConversation(id);
        setContext(nextContext);
        setContextError("");
        setChild(null);
        setPickedTask(null);
        setSelectedReply(null);
        setStatus("");
        window.setTimeout(() => box.current?.focus(), 0);
    }

    // Deleting a thread takes its tasks, schedules and agent sessions with
    // it, so the reader is moved to another thread rather than left looking
    // at a transcript that no longer exists. A refusal is reported by the
    // dialog that asked, so it is not swallowed here.
    async function removeConversation(id: string) {
        await deleteConversation(id);
        const remaining = conversations.filter((c) => c.id !== id);
        setConversations(remaining);
        loadConversations();
        if (id !== activeConversation.current) return;
        if (remaining.length > 0) selectConversation(remaining[0].id);
        else await newSession();
    }

    // startingProject is what a fresh thread opens in when nothing else
    // says: the thread being read, else the default project, else the one
    // project there is. A console that knows a project never refuses to
    // open a thread for want of one.
    function startingProject() {
        return context?.project?.id || snap.projects.find((p) => p.default)?.id || snap.projects.find((p) => !p.home)?.id || snap.projects[0]?.id;
    }

    async function newSession(project = startingProject(), prompt = "") {
        if (creatingRequest.current) return;
        setMobileSessions(false);
        creatingRequest.current = true;
        setCreating(true);
        setStatus("");
        const from = conversation;
        const id = `console:${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
        try {
            if (!project) throw new Error(t("console.projectNotReady"));
            await initializeConversation(id, project);
            const data = await fetchContext(id);
            if (data.context?.project?.id !== project || !data.context.project.bound) throw new Error(t("console.bindingFailed"));
            // Do not pull the reader out of a different thread chosen while binding.
            if (activeConversation.current === from) {
                selectConversation(id, data.context);
                // A prepared prompt is a draft, not a submission: the reader
                // reads it, edits it, and decides when it is sent.
                if (prompt) { writeDraft(id, prompt); window.setTimeout(() => box.current?.focus(), 0); }
            }
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
            if (context?.project?.id) await initializeConversation(id, context.project.id);
            await deleteQueued(q.id);
            try { await enqueue(id, q.input, q.quotes); }
            catch (e) { writeDraft(id, q.input); setQuotes(q.quotes || []); selectConversation(id); throw e; }
            selectConversation(id);
        });
    }

    async function deliver(pending: Submission) {
        try {
            await enqueue(conversation, pending.input, pending.quotes.length ? pending.quotes : undefined, pending.id, submissionRefs(pending), pending.locale, pending.rewind);
            // The thread is back at the edited line and the lines after it
            // are gone, so what is drawn is refetched rather than patched.
            if (pending.rewind) { await endRewind(conversation); void invalidateReplies(); }
            const recorded = await finishSubmission(conversation, pending.id);
            if (!recorded && activeConversation.current === conversation) setStatus(t("console.receiptStorage"));
            if (activeConversation.current === conversation) setSelectedReply(null);
        } catch (error) {
            const conflict = error instanceof HTTPError && error.status === 409;
            const message = error instanceof Error ? error.message : String(error);
            // A rewind whose target the hub no longer has can never
            // succeed; keeping it armed would fail every later send.
            const gone = !!pending.rewind && error instanceof HTTPError && error.status === 404;
            if (gone) await endRewind(conversation);
            const retained = await failSubmission(conversation, pending.id, message, conflict ? "conflict" : isRejectedRequest(error) ? "rejected" : "unknown");
            if (activeConversation.current === conversation) setStatus(gone ? t("console.rewindGone") : !retained ? t("console.receiptStorage") : isRejectedRequest(error) && !pending.uncertain ? message : "");
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
        // A verb chip or a picker sends its own line; only what the person
        // typed replaces the message they are editing.
        const armed = line === undefined ? rewindOf(conversation)?.reply : undefined;
        try { pending = await beginSubmission(conversation, input, quotes, line === undefined, locale, line === undefined, armed); }
        catch { if (activeConversation.current === conversation) setStatus(t("console.pendingStorage")); return; }
        if (!pending) return;
        clearStopNotice(conversation);
        if (activeConversation.current === conversation) { setStatus(""); followTranscript.current = true; }
        await deliver(pending);
    }

    async function retrySend() {
        if (!canSubmit || isStopPending(conversation)) return;
        let pending: Submission | null;
        try { pending = await retrySubmission(conversation); }
        catch { if (activeConversation.current === conversation) setStatus(t("console.retryStorage")); return; }
        if (pending) { if (activeConversation.current === conversation) setStatus(""); await deliver(pending); }
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

    // The roots of this conversation's call graph: its tasks whose parent
    // is not itself one of them.
    const { runningPlans, roots } = useMemo(() => {
        const settled = ["done", "failed", "skipped", "cancelled"];
        const mine = snap.tasks.filter((t) => t.channel === conversation);
        const plans = snap.plans.filter((p) => {
            const task = mine.find((t) => t.id === p.task_id);
            return task && (p.steps || []).some((s) => !settled.includes(s.state));
        });
        const ids = new Set(mine.map((t) => t.id));
        return { runningPlans: plans, roots: mine.filter((t) => !t.parent || !ids.has(t.parent)).sort((a, b) => (b.updated_at || "").localeCompare(a.updated_at || "")) };
    }, [snap.tasks, snap.plans, conversation]);

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
    function onKey(e: KeyboardEvent) {
        if (e.isComposing) return;
        if (suggestions.length) {
            if (e.key === "ArrowDown") { e.preventDefault(); setPick((i) => (i + 1) % suggestions.length); return; }
            if (e.key === "ArrowUp") { e.preventDefault(); setPick((i) => (i - 1 + suggestions.length) % suggestions.length); return; }
            if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey && suggestions[pick] && suggestions[pick].insert !== text)) {
                e.preventDefault(); apply(suggestions[pick]); return;
            }
            if (e.key === "Escape") { e.preventDefault(); setSuggestions([]); return; }
        }
        // Enter sends, Shift+Enter is a newline. The composer has already
        // swallowed the Enter an input method confirms a candidate with.
        if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); void submit(); }
    }

    const lastWithProcess = [...entries].reverse().find((r) => r.kind === "reply" && (r.process || r.injected));
    const shownProcess = (selectedReply ? transcript.find((r) => r.id === selectedReply.id) : undefined) ?? (lastWithProcess ? withDelegations(lastWithProcess, delegations) : null);
    const recordedSteps = new Set(entries.flatMap((r) => r.process?.steps?.map((s) => s.id) || []));
    const unrecordedChildren = Object.values(delegations).map(({ step }) => step).filter((s) => !recordedSteps.has(s.id));
    // Children the replies have not recorded yet still belong where they
    // started: this turn's inside the line in flight, older ones back
    // among the replies they were handed over from.
    const { current: turnChildren, earlier: earlierChildren } = childrenOfTurn(unrecordedChildren, live?.since);
    // How much of the thread a send would take back, counted from what is
    // drawn: the lines under the message being edited.
    const rewindView = useMemo(() => {
        if (!rewind) return undefined;
        const at = transcript.findIndex((r) => r.id === rewind.reply);
        return { following: at < 0 ? 0 : transcript.length - at - 1 };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [rewind, entries, delegations]);
    const current = conversations.find((c) => c.id === conversation);
    const title = current?.title || (entries.find((r) => r.kind === "sent" && r.input?.trim() && !r.input.trim().startsWith("/"))?.input?.split("\n")[0]) || t("console.newConversation");
    useEffect(() => { materials.setTarget(context?.project ? { conversation, project: context.project.id, title } : null); }, [conversation, context?.project?.id, title, materials.setTarget]);
    async function upload(files: FileList | File[] | null) {
        const chosen = Array.from(files ?? []);
        if (!chosen.length || uploading || !context?.project || !submissionSupport.material_refs) return;
        const target = { conversation, project: context.project.id, title };
        setUploading(true); setStatus("");
        try { for (const file of chosen) { const material = await uploadMaterial(target.project, file, locale); await materials.add(material, { id: material.id }, target); } }
        catch (error) { setStatus(error instanceof Error ? error.message : String(error)); }
        finally { setUploading(false); }
    }
    const toolbarStatus = stopState?.uncertain ? t("console.stopUncertain") : status || contextError || conversationsError || (queueError ? readErrorText(queueError) : "") || (replyError ? readErrorText(replyError) : "") || stopState?.error || stopState?.message || (creating ? t("console.creating") : stopping ? t("console.stopping") : submission?.active ? t("console.sending") : recoveryState ? t(recoveryState === "recovering" ? "console.recovering" : "status.awaitingHuman") : live || busy ? t("console.working") : "");
    const listed = useMemo(() => conversations.some((c) => c.id === conversation) ? conversations : [{ id: conversation, title: t("console.newConversation"), last_at: "", count: 0, running: false, project: context?.project?.id, agent: context?.agent?.id }, ...conversations], [conversations, conversation, t, context?.project?.id, context?.agent?.id]);
    const agents = useMemo(() => context?.agents ?? [], [context?.agents]);

    // Handlers keep one identity for the life of the page and always run
    // the current closure; a memoised column is then only drawn again when
    // something it shows has actually changed.
    const pickConversation = useEventCallback((id: string) => selectConversation(id));
    const openNewSession = useEventCallback((project?: string) => void newSession(project || startingProject()));
    const openImport = useEventCallback(() => { setMobileSessions(false); setImporting(true); });
    const patchConversation = useEventCallback((id: string, patch: { title?: string; archived?: boolean }) => void updateConversation(id, patch).then(loadConversations).catch((e) => setStatus(String(e).replace(/^Error: /, ""))));
    const dropConversation = useEventCallback((id: string) => removeConversation(id));
    const toggleSessions = useEventCallback(() => desktopSessions ? setSessionsCollapsed(!sessionsCollapsed) : setMobileSessions(false));
    const openTask = useEventCallback((task: Task) => { setMobileSessions(false); if (task.parent && stepOf(task.id)) { setChild(task); setPickedTask(null); } else setPickedTask(task); });
    const closeInspector = useEventCallback(() => setInspectorOpen(false));
    const selectReply = useEventCallback((r: Reply) => { setSelectedReply(r); setTab("trace"); setInspectorOpen(true); });
    const placeDraft = (value: string) => {
        setText(value);
        window.setTimeout(() => { box.current?.focus(); box.current?.caretToEnd(); }, 0);
    };
    // Editing a line already sent arms a rewind: the thread will go back
    // to that line when the replacement is sent. Arming is remembered
    // outside React so a reload mid-edit cannot turn the rewrite into an
    // ordinary message appended under the original.
    async function armRewind(reply: string, value: string) {
        await beginRewind(conversation, reply, text);
        placeDraft(value);
    }
    const editSent = useEventCallback((r: Reply) => {
        const value = (r.input || "").trim();
        if (!value || !r.id) return;
        if (busy || submission?.active || stopping || isStopPending(conversation)) { setStatus(t("console.editBusy")); return; }
        setStatus("");
        if (text.trim() && text.trim() !== value) { setReplacingDraft({ reply: r.id, text: value }); return; }
        void armRewind(r.id, value);
    });
    const dropRewind = useEventCallback(async () => {
        const armed = await endRewind(conversation);
        setStatus("");
        if (armed) placeDraft(armed.restore);
    });
    const quoteReply = useEventCallback((r: Reply) => { if (!r.id) return; void setQuotes((list) => list.some((x) => x.reply_id === r.id) ? list : [...list, { conversation, reply_id: r.id!, title: current?.title || conversation, excerpt: (r.text || "").replace(/\s+/g, " ").slice(0, 80) }]); });
    const changeText = useEventCallback((value: string) => setText(value));
    // Pasting a screenshot into the box attaches it, the way the attach
    // button would; when the agent cannot take attachments the paste says
    // so instead of being swallowed.
    const pasteFiles = useEventCallback((files: File[]) => {
        if (!files.length) return;
        if (!submissionSupport.material_refs) { setStatus(t("materials.pasteUnsupported")); return; }
        if (!context?.project) { setStatus(t("materials.noTarget")); return; }
        void upload(files.map(stamped));
    });
    const submitLine = useEventCallback(() => void submit());
    const stopLine = useEventCallback(() => void stop());
    const dropQuote = useEventCallback((q: { reply_id: string }) => void setQuotes((list) => list.filter((y) => y.reply_id !== q.reply_id)));
    const toggleQueueing = useEventCallback(() => setQueueing(!queueing));
    const steerLine = useEventCallback((q: Queued) => steer(q));
    const dropQueued = useEventCallback((q: Queued) => void queueAction(() => deleteQueued(q.id)));
    const editQueuedLine = useEventCallback(async (q: Queued, input: string) => { try { await editQueued(q.id, input); } finally { await loadQueue(); } });
    const openSideChat = useEventCallback((q: Queued) => void sideChat(q));
    const applySuggestion = useEventCallback((item: Suggestion) => apply(item));
    const runVerb = useEventCallback((cmd: string) => { setText(cmd + " "); box.current?.focus(); });
    const chooseProject = useEventCallback((id: string) => void submit(`/project use ${id}`));
    const chooseAgent = useEventCallback((id: string) => void submit(`/use ${id}`));
    const pressKey = useEventCallback((e: KeyboardEvent) => onKey(e));
    const loadSelectors = useEventCallback(() => fetchSelectors(conversation, context!.agent!.id));
    const prefer = useEventCallback(async (patch: Record<string, string>) => { if (!context?.agent) return; const running = !!live; const result = await setPreferences(conversation, context.agent.id, patch); if (activeConversation.current === conversation) { setStatus(t(result.live ? "console.preferenceLive" : running ? "console.preferenceNextTurn" : "console.preferenceSaved")); loadContext(); } });

    const sessions = (collapsed = sessionsCollapsed, resizable = true) => <SessionsTree resizable={resizable} list={listed} projects={snap.projects} current={conversation} onPick={pickConversation} onNew={openNewSession} creating={creating} onImport={openImport}
                onUpdate={patchConversation}
                onDelete={dropConversation}
                collapsed={collapsed} onToggle={toggleSessions}
                tasks={snap.tasks} onTask={openTask} />;
    const inspector = <Rail key={conversation} context={context} live={live} plans={runningPlans} reply={shownProcess} tab={tab} setTab={setTab} roots={roots} onClose={closeInspector} />;
    const splitLabel = (tab: SplitTab) => tab.kind === "chat" ? t("sideChat.title") : t("consoleChrome.delegation", { id: "#" + tab.task });
    const closeSplitTab = (tab: SplitTab) => { if (tab.kind === "chat") side.close(); else split.close(tab.id); };
    const renderSplitTab = (tab: SplitTab) => tab.kind === "chat"
        ? <SideChatPanel embedded />
        : <DelegationPanel id={"#" + tab.task} info={stepOf(tab.task!)} progress={stepOf(tab.task!)} />;
    const splitOpen = !reviewing && split.tabs.length > 0 && !split.hidden;
    // Right to left: the details rail stands furthest right, then the
    // pane beside the conversation. The session list on the far left is
    // navigation rather than something opened, so it stays put.
    useCloseLayer(closeOrder.inspector, () => setInspectorOpen(false), inspectorOpen && dockInspector && !reviewing);
    useCloseLayer(closeOrder.splitTab, () => { const tab = split.tabs.find((item) => item.id === split.active) || split.tabs.at(-1); if (tab) closeSplitTab(tab); }, splitOpen);
    return (
        <div className="console-workbench">
            {replacingDraft !== null && <ConfirmDialog title={t("console.replaceDraftTitle")} body={t("console.replaceDraftBody")} confirmLabel={t("console.replaceDraft")}
                onConfirm={() => armRewind(replacingDraft.reply, replacingDraft.text)} onClose={() => setReplacingDraft(null)} />}
            {importing && <NativeSessionImport agents={snap.agents} nodes={snap.nodes} projects={snap.projects} onClose={() => setImporting(false)} onImported={(id) => { setImporting(false); refresh(); selectConversation(id); void loadConversations(); }} />}
            {view === "chat" && desktopSessions && !side.session && sessions()}
            {mobileSessions && !desktopSessions && <Sheet label={t("console.sessions")}  side="left" width={300} onClose={() => setMobileSessions(false)}><button type="button" className="sheet-close workbench-icon-button" aria-label={t("console.closeSessions")}  onClick={() => setMobileSessions(false)}><X aria-hidden="true" /></button>{sessions(false, false)}</Sheet>}
            {pickedTask && <TaskDrawer t={pickedTask} tasks={snap.tasks} plan={snap.plans.find((p) => p.task_id === pickedTask.id)} onClose={() => setPickedTask(null)} width={RAIL_WIDTH} />}

            <div className="console-main">
                {hubUpdated && <div role="status" className="border-b border-secondary bg-warning-primary px-6 py-2 text-sm text-secondary">
                    {t("console.hubUpdated")}<button type="button" className="underline" onClick={() => { if (window.confirm(t("console.reloadConfirm"))) window.location.reload(); }}>{t("console.reloadPage")}</button>
                </div>}
                {view === "chat" && <header className="console-toolbar">
                    <button type="button" className="workbench-icon-button" aria-label={t("console.sessions")}  title={t("console.sessions")}  onClick={() => desktopSessions ? setSessionsCollapsed(!sessionsCollapsed) : setMobileSessions(true)}><LayoutLeft aria-hidden="true" /></button>
                    <div className="console-heading">
                    <h1 title={title}>{title}</h1>
                    {context?.project && <div className="console-location" title={context.agent?.place ? placeLabel(context.agent.place, locale, nodeLabelOf) : context.project.path}>{context.project.id} · {context.agent?.place ? placeLabel(context.agent.place, locale, nodeLabelOf) : nodeLabelOf(context.project.node)}</div>}
                    </div>
                    {current?.archived && (
                        <span className="flex items-center gap-1.5">
                            <Badge type="pill-color" size="sm" color="gray">{t("console.archived")}</Badge>
                            <button type="button" className="text-xs text-tertiary hover:text-primary" onClick={() => void updateConversation(current.id, { archived: false }).then(loadConversations).catch((e) => setStatus(String(e).replace(/^Error: /, "")))}>{t("console.unarchive")}</button>
                        </span>
                    )}
                    <span role="status" className="console-status" title={toolbarStatus}>{toolbarStatus}</span>
                    <ThemeMenu />
                    <span className="workbench-segmented" role="group" aria-label={t("console.workView")} >
                        <button type="button" onClick={() => navigate("/console")} aria-pressed>{t("console.conversation")}</button>
                        <button type="button" onClick={() => navigate("/console?view=board")} aria-pressed={false}>{t("console.board")}</button>
                    </span>
                    {view === "chat" && split.tabs.length > 0 && <button type="button" className="workbench-icon-button" aria-label={split.hidden ? t("split.show", { count: split.tabs.length }) : t("split.hide")} aria-pressed={!split.hidden} title={split.hidden ? t("split.show", { count: split.tabs.length }) : t("split.hide")} onClick={() => split.setHidden(!split.hidden)}><Columns03 aria-hidden="true" /></button>}
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
                                {streamWithChildren(transcript, earlierChildren).map(({ reply: r, child }, i) => child
                                    ? <DelegationCard key={child.id} id={child.id} info={child} progress={child} />
                                    : r!.kind === "sent" ? <UserMessage key={r!.id || i} r={r!} onEdit={editSent} /> : <AssistantMessage key={r!.id || i} r={r!} selected={shownProcess?.id === r!.id} onSelect={selectReply} onQuote={quoteReply} />)}
                                {live && <Working live={live} plans={runningPlans} compact delegated={turnChildren} recovery={recoveryState} />}
                            </div>
                        </div>
                        <div className="composer-dock">
                            {draftIssue && <div role="alert" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary"><p>{t(draftIssue === "conflict" ? "console.draftConflict" : draftIssue === "unavailable" ? "console.draftLockUnavailable" : "console.draftStorage")}</p>{draftIssue === "conflict" && <><pre className="mt-2 max-h-24 overflow-auto whitespace-pre-wrap break-words">{savedDraft}</pre><div className="mt-2 flex flex-wrap gap-3"><button type="button" className="underline" onClick={() => void resolveDraftConflict(conversation, "local")}>{t("console.keepLocalDraft")}</button><button type="button" className="underline" onClick={() => void resolveDraftConflict(conversation, "remote")}>{t("console.useSavedDraft")}</button></div></>}</div>}
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
                                {(!submission.id || submission.rejected) && <button type="button" className="mr-4 underline" onClick={async () => { if (!await restoreSubmission(conversation)) setStatus(t("console.recoveryStorage")); }}>{t("console.restoreDraft")}</button>}
                                <button type="button" className="underline" onClick={async () => { if (await finishSubmission(conversation, submission.id)) setStatus(""); }}>{t("console.received")}</button>
                            </div>}
                            {stopState?.uncertain && <div role="alert" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary">
                                <p>{t("console.stopUncertain")}</p>
                                {stopState.error && <details className="mt-2"><summary className="cursor-pointer text-xs text-tertiary">{t("console.details")}</summary><pre className="mt-2 max-h-24 overflow-auto whitespace-pre-wrap break-words text-xs [overflow-wrap:anywhere]">{stopState.error}</pre></details>}
                                <button type="button" className="mt-1 underline" onClick={() => void stop()}>{t("console.retryStop")}</button>
                            </div>}
                            {submissionSupport.interactive_requests && <QuestionPanel key={conversation} conversation={conversation} turnStartedAt={live?.since} />}
                            {draftMaterials.length > 0 && <ul aria-label={t("materials.draftRefs")} className="mx-auto mb-2 flex max-w-3xl flex-wrap gap-2">{draftMaterials.map((ref) => <li key={refKey(ref)} className="flex max-w-full items-center gap-2 rounded-lg bg-secondary px-3 py-2 text-xs"><button type="button" className="truncate" onClick={() => setOpenedMaterial(ref)}>{ref.title}{ref.selector?.kind === "lines" ? ` · L${ref.selector.start}–L${ref.selector.end}` : ""}</button><button type="button" aria-label={t("materials.remove", { title: ref.title })} onClick={async () => { if (!await removeDraftMaterial(conversation, ref)) setStatus(t("materials.sourceUnavailable")); }}>×</button></li>)}</ul>}
                            {submissionSupport.material_refs && <div className="mx-auto mb-2 flex max-w-3xl flex-wrap items-center gap-3"><label className="cursor-pointer rounded-md px-2 py-1 text-xs text-tertiary hover:bg-secondary">{uploading ? t("materials.uploading") : t("materials.upload")}<input type="file" multiple className="sr-only" disabled={uploading} aria-label={t("materials.upload")} onChange={(event) => { void upload(event.target.files); event.target.value = ""; }} /></label><span className="text-xs text-quaternary">{t("materials.uploadHint")}</span></div>}
                            {openedMaterial && context?.project && <MaterialPreview project={context.project.id} anchor={openedMaterial} onClose={() => setOpenedMaterial(null)} />}
                            <Composer
                                value={text} hasMaterials={draftMaterials.length > 0} onChange={changeText} onPasteFiles={pasteFiles} onSubmit={submitLine} onStop={stopLine}
                                busy={busy} pending={!!submission} stopping={stopping} disabled={creating || !context || !canSubmit} boxRef={box} onKey={pressKey}
                                quotes={quotes} onDropQuote={dropQuote}
                                rewind={rewindView} onCancelRewind={dropRewind}
                                queue={queue} queueing={queueing} onToggleQueueing={toggleQueueing}
                                onSteer={steerLine} onDropQueued={dropQueued}
                                onEditQueued={editQueuedLine}
                                onSideChat={openSideChat}
                                suggestions={suggestions} pick={pick} onApply={applySuggestion}
                                verbs={verbs} onVerb={runVerb}
                                projects={snap.projects} project={context?.project} onProject={chooseProject}
                                agents={agents} agent={context?.agent} onAgent={chooseAgent}
                                preferenceKey={`${conversation}:${context?.agent?.id || ""}`}
                                onSelectors={context?.agent ? loadSelectors : undefined}
                                onPrefer={prefer}
                            />
                            {(busy || stopping) && <p role="status" className="composer-running"><span>{stopping ? t("console.stopping") : recoveryState === "recovering" ? t("console.recovering") : recoveryState ? t("status.awaitingHuman") : t("console.runningNow")}</span></p>}
                        </div>
                    </div>
                    {splitOpen && <SplitPane tabs={split.tabs} active={split.active} label={splitLabel} onFocus={split.focus} onClose={closeSplitTab} onHide={() => split.setHidden(true)} render={renderSplitTab} />}
                    {inspectorOpen && dockInspector && <ResizableInspector>{inspector}</ResizableInspector>}
                {inspectorOpen && !dockInspector && <Sheet label={t("console.details")}  width={resizeInspector ? "max-content" : 360} onClose={() => setInspectorOpen(false)}>{resizeInspector ? <ResizableInspector overlay>{inspector}</ResizableInspector> : inspector}</Sheet>}
                </div>
                )}
            </div>
        </div>
    );
}
