import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { X } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { TextArea } from "@/components/base/textarea/textarea";
import { useSideChat, type SideSession } from "@/providers/side-chat-provider";
import { useI18n } from "@/providers/locale-provider";
import { isStreamingProgress, useConsoleEvents, useFleet } from "@/lib/fleet";
import { useResourceRead } from "@/hooks/use-resource-read";
import { fetchQueue, fetchReplies, enqueue, send, getSubmissionSupport, subscribeSubmissionSupport } from "@/lib/api/console";
import { useDraft, useDraftIssue, useSavedDraft, resolveDraftConflict, useMaterials, useSubmission, updateDraft, removeDraftMaterial, beginSubmission, retrySubmission, finishSubmission, failSubmission, reconcileSubmission, restoreSubmission, submissionRefs, useStops, beginStop, finishStop, isStopPending, type Submission } from "@/lib/drafts";
import { HTTPError, isRejectedRequest } from "@/lib/http";
import { refKey } from "@/lib/material-ref";
import type { Exchange, Reply } from "@/lib/types";
import { Md } from "./markdown";
import { Working } from "./trace";
import { applyLive, type Live } from "@/lib/live";
import { QuestionPanel } from "./question-panel";
import "@/styles/side-chat.css";

export function SideChatPanel({ onOpenMain }: { onOpenMain?: () => void } = {}) { const { session } = useSideChat(); return session ? <SideConversation key={session.id} session={session} onOpenMain={onOpenMain} /> : null; }
function SideConversation({ session, onOpenMain }: { session: SideSession; onOpenMain?: () => void }) {
    const { t, locale } = useI18n(); const side = useSideChat(); const { live } = useFleet();
    const consoleEvents = useConsoleEvents();
    const draftIssue = useDraftIssue(session.id);
    const savedDraft = useSavedDraft(session.id);
    const text = useDraft(session.id), refs = useMaterials(session.id), pending = useSubmission(session.id), stops = useStops(), stop = stops[session.id];
    const support = useSyncExternalStore(subscribeSubmissionSupport, getSubmissionSupport);
    const [replies, setReplies] = useState<Reply[]>([]), [queue, setQueue] = useState<Exchange[]>([]), [readError, setReadError] = useState(""), [loaded, setLoaded] = useState(false);
    const [turn, setTurn] = useState<Live | null>(null); const seen = useRef(0);
    const input = useRef<HTMLTextAreaElement>(null), transcript = useRef<HTMLDivElement>(null), follow = useRef(true);
    const load = useResourceRead(`side:${session.id}`, async (signal) => Promise.all([fetchReplies(session.id, signal), fetchQueue(session.id, signal)]), ([history, exchanges]) => { setReplies(history.replies || []); setQueue(exchanges.queue || []); reconcileSubmission(session.id, exchanges.queue || []); setReadError(""); setLoaded(true); }, (error) => { setReadError(error instanceof Error ? error.message : String(error)); setLoaded(true); });
    const event = consoleEvents.findLast((entry) => entry.conversation === session.id && !isStreamingProgress(entry))?.n;
    useEffect(() => { void load(); const timer = window.setInterval(() => void load(), 3000); return () => window.clearInterval(timer); }, [load, live]);
    useEffect(() => { void load(); }, [event, load]);
    // The side conversation streams like the main one: its progress events
    // fold into a live view that the reply replaces. The buffer is trimmed
    // from the front, so the cursor is an arrival number, not an index.
    useEffect(() => {
        const fresh = consoleEvents.filter((entry) => (entry.n ?? 0) > seen.current);
        if (!fresh.length) return;
        seen.current = fresh[fresh.length - 1].n ?? seen.current;
        const mine = fresh.filter((entry) => entry.conversation === session.id);
        if (mine.length) setTurn((current) => mine.reduce(applyLive, current));
    }, [consoleEvents, session.id]);
    useEffect(() => { if (follow.current && transcript.current) transcript.current.scrollTop = transcript.current.scrollHeight; }, [replies, queue, turn]);
    useEffect(() => { if (stop && !stop.active) void load(); }, [stop, load]);
    const busy = queue.some((entry) => ["running", "recovering", "awaiting-user"].includes(entry.state));
    const blocked = support.state !== "supported" || (refs.length > 0 && !support.material_refs);
    const focused = useRef(false);
    useEffect(() => { if (!blocked && !focused.current && input.current) { input.current.focus({ preventScroll: true }); focused.current = true; } }, [blocked]);
    async function deliver(submission: Submission) {
        try {
            await side.ensureBound(session);
            await enqueue(session.id, submission.input, [], submission.id, submissionRefs(submission), submission.locale);
            const recorded = await finishSubmission(session.id, submission.id); side.report(session.id, recorded ? "" : t("console.receiptStorage"));
        } catch (error) {
            const outcome = error instanceof HTTPError && error.status === 409 ? "conflict" : isRejectedRequest(error) ? "rejected" : "unknown";
            const message = error instanceof Error ? error.message : String(error);
            const recorded = await failSubmission(session.id, submission.id, message, outcome); side.report(session.id, recorded ? message : t("console.receiptStorage"));
        } finally { void load(); }
    }
    async function submit(retry = false) {
        if (isStopPending(session.id) || blocked || (!retry && (!text.trim() && refs.length === 0))) return;
        try { const submission = retry ? await retrySubmission(session.id) : await beginSubmission(session.id, text.trim(), [], true, locale); if (submission) { side.report(session.id, ""); follow.current = true; await deliver(submission); } }
        catch { side.report(session.id, t("sideChat.storage")); }
    }
    async function cancel() {
        const id = beginStop(session.id); if (!id) return;
        try { const reply = await send(session.id, "/cancel", undefined, id); finishStop(session.id, id, { message: reply.text || t("sideChat.stopped") }); }
        catch (error) { finishStop(session.id, id, { error: error instanceof Error ? error.message : String(error), uncertain: !isRejectedRequest(error) }); }
        finally { void load(); }
    }
    return <section data-side-chat className="side-chat" role="region" aria-label={t("sideChat.region")}>
        <header className="side-chat-header"><div className="min-w-0 flex-1"><h2 className="text-sm font-semibold">{t("sideChat.title")}</h2><p className="truncate text-xs text-tertiary" title={session.title}>{session.project} · {session.title}</p></div><button type="button" className="workbench-icon-button" aria-label={t("sideChat.close")} onClick={side.close}><X aria-hidden="true" /></button></header>
        <a href={`#/console?conversation=${encodeURIComponent(session.id)}`} className="mx-4 mb-2 self-start text-xs text-tertiary underline" onClick={() => { side.close(); onOpenMain?.(); }}>{t("sideChat.openMain")}</a>
        <div ref={transcript} className="side-chat-transcript" onScroll={(event) => { const node = event.currentTarget; follow.current = node.scrollHeight - node.clientHeight - node.scrollTop < 48; }}>
            {session.excerpt && <details className="mb-3 rounded-lg bg-secondary p-3 text-xs"><summary className="cursor-pointer text-tertiary">{t("sideChat.source")}</summary><p className="mt-2 whitespace-pre-wrap break-words">{session.excerpt}</p></details>}
            {replies.map((reply, index) => <article key={reply.id || index} className={reply.kind === "sent" ? "side-chat-user" : "side-chat-answer"}>{reply.kind === "sent" ? <Md text={reply.input || ""} /> : reply.format === "text" ? <p className="whitespace-pre-wrap break-words">{reply.text}</p> : <Md text={reply.text} />}{reply.error && <p className="mt-1 text-xs text-error-primary">{reply.error}</p>}</article>)}
            {queue.filter((entry) => entry.state === "queued" && !replies.some((reply) => reply.exchange_id === entry.id && reply.kind === "sent")).map((entry) => <article key={entry.id} className="side-chat-user"><p className="whitespace-pre-wrap break-words">{entry.input || t("sideChat.materialCount", { count: entry.refs?.length || 0 })}</p><span className="mt-1 block text-xs text-tertiary">{t("sideChat.queued")}</span></article>)}
            {!replies.length && !queue.length && <p className="py-4 text-xs leading-5 text-tertiary">{t(loaded ? "sideChat.empty" : "sideChat.loading")}</p>}
            {turn ? <Working live={turn} plans={[]} compact /> : busy && <p role="status" className="text-xs text-tertiary">{t("sideChat.running")}</p>}
            {readError && <p role="alert" className="text-xs text-error-primary">{readError}<button type="button" className="ml-2 underline" onClick={() => void load()}>{t("sideChat.retry")}</button></p>}
        </div>
        {support.interactive_requests && <QuestionPanel conversation={session.id} />}
        <div className="side-chat-compose">
            {draftIssue && <div role="alert" className="mb-2 rounded-lg bg-warning-primary p-3 text-xs"><p>{t(draftIssue === "conflict" ? "console.draftConflict" : draftIssue === "unavailable" ? "console.draftLockUnavailable" : "console.draftStorage")}</p>{draftIssue === "conflict" && <><pre className="mt-2 max-h-24 overflow-auto whitespace-pre-wrap break-words">{savedDraft}</pre><div className="mt-2 flex flex-wrap gap-3"><button type="button" className="underline" onClick={() => void resolveDraftConflict(session.id, "local")}>{t("console.keepLocalDraft")}</button><button type="button" className="underline" onClick={() => void resolveDraftConflict(session.id, "remote")}>{t("console.useSavedDraft")}</button></div></>}</div>}
            {refs.length > 0 && <ul className="mb-2 flex flex-wrap gap-1" aria-label={t("materials.draftRefs")}>{refs.map((ref) => <li key={refKey(ref)} className="flex max-w-full items-center gap-1 rounded bg-secondary px-2 py-1 text-xs"><span className="truncate">{ref.title}</span><button type="button" aria-label={t("sideChat.removeRef", { title: ref.title })} onClick={async () => { if (!await removeDraftMaterial(session.id, ref)) side.report(session.id, t("sideChat.storage")); }}>×</button></li>)}</ul>}
            {pending && !pending.active && <div role="alert" className="mb-2 rounded-lg bg-warning-primary p-3 text-xs"><p>{t(pending.conflict ? "sideChat.conflict" : pending.rejected ? "sideChat.rejected" : "sideChat.unknown")}</p><p className="my-1 whitespace-pre-wrap break-words">{pending.input}</p>{pending.error && <p className="mb-2 break-words">{pending.error}</p>}{pending.id && !pending.conflict && !pending.rejected && <button type="button" className="mr-3 underline" onClick={() => void submit(true)}>{t("sideChat.retrySend")}</button>}{(!pending.id || pending.rejected) && <button type="button" className="mr-3 underline" onClick={() => restoreSubmission(session.id)}>{t("sideChat.restore")}</button>}<button type="button" className="underline" onClick={() => finishSubmission(session.id, pending.id)}>{t("sideChat.confirmed")}</button></div>}
            <TextArea textAreaRef={input} aria-label={t("sideChat.message")} placeholder={t("sideChat.placeholder")} value={text} onChange={(value) => { void updateDraft(session.id, value); }} rows={3} isDisabled={blocked} onKeyDown={(event) => { if (event.nativeEvent.isComposing) return; if (event.key === "Enter" && event.shiftKey) { event.preventDefault(); void submit(); } }} />
            <div className="mt-2 flex items-center gap-2"><span role="status" className="min-w-0 flex-1 text-xs text-tertiary">{blocked ? t("sideChat.unavailable") : session.binding ? t("sideChat.binding") : pending?.active ? t("sideChat.sending") : stop?.error || stop?.message || ""}</span>{(busy || stop?.uncertain) && <Button size="sm" color="secondary" isDisabled={stop?.active} onClick={() => void cancel()}>{t(stop?.active ? "sideChat.stopping" : "sideChat.stop")}</Button>}<Button size="sm" isDisabled={blocked || !!pending || isStopPending(session.id) || (!text.trim() && !refs.length)} onClick={() => void submit()}>{t(busy ? "sideChat.queue" : "sideChat.send")}</Button></div>
            {session.agentReviewRequired && <div className="mt-2 space-y-2 text-xs"><p>{t("sideChat.chooseAgent")}</p><Button size="sm" color="secondary" onClick={() => void side.acceptWorkbenchAgent(session).catch((error) => side.report(session.id, error instanceof Error ? error.message : String(error)))}>{t("sideChat.checkChosenAgent")}</Button></div>}
            {session.error && !pending && <p role="alert" className="mt-2 text-xs text-error-primary">{session.error}</p>}{stop?.uncertain && <p role="alert" className="mt-2 text-xs text-error-primary">{t("sideChat.stopUnknown")}</p>}
        </div>
    </section>;
}
