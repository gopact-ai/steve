import { useEffect, useId, useRef, useState } from "react";
import { Button as AriaButton, Radio, RadioGroup } from "react-aria-components";
import { ChevronDown } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { TextArea } from "@/components/base/textarea/textarea";
import { Md } from "@/components/steve/markdown";
import { questions, answerQuestion } from "@/lib/api/material";
import { fetchSelectors, setPreferences } from "@/lib/api/console";
import { useNodeLabel, whoIs } from "@/lib/node-name";
import type { PendingQuestion, QuestionAnswer, Selectors } from "@/lib/types";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useConsoleEvents, useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import { HTTPError } from "@/lib/http";
import { dateTime } from "@/lib/format";
import "@/styles/questions.css";

export function QuestionPanel({ conversation }: { conversation: string }) {
    const { t } = useI18n();
    const { live } = useFleet();
    const [items, setItems] = useState<PendingQuestion[]>([]);
    const [error, setError] = useState("");
    const load = useResourceRead(`questions:${conversation}`, (signal) => questions(conversation, signal), (value) => {
        setItems((previous) => {
            const resolved = new Map(previous.filter((question) => question.answer?.command_id && ["answered", "declined", "cancelled"].includes(question.state)).map((question) => [question.id, question]));
            // Preserve acknowledged decisions against stale reads. An unanswered
            // interruption or cancellation can return to pending after recovery.
            return (value.questions || []).map((question) => resolved.get(question.id) || { ...question, options: question.options || [] });
        });
        setError("");
    }, (error) => setError(String(error)));
    useEffect(() => { void load(); const timer = window.setInterval(() => void load(), 5000); return () => window.clearInterval(timer); }, [conversation, live, load]);
    useConsoleEvents((fresh) => { if (fresh.some((event) => event.kind === "console.question" && event.conversation === conversation)) void load(); });
    const pending = items.filter((question) => question.state === "pending").sort((a, b) => a.created_at.localeCompare(b.created_at));
    const resolved = items.filter((question) => question.state !== "pending").sort((a, b) => b.updated_at.localeCompare(a.updated_at));
    // The newest decisions are the ones worth seeing first, but a thread
    // that has answered forty requests must still be able to reach the
    // first of them, so the rest are a click away rather than cut off.
    const [depth, setDepth] = useState(5);
    useEffect(() => setDepth(5), [conversation]);
    const history = resolved.slice(0, depth);
    function acknowledge(question: PendingQuestion) {
        setItems((previous) => previous.map((item) => item.id === question.id ? { ...question, options: question.options || [] } : item));
        void load();
    }
    if (pending.length === 0 && resolved.length === 0 && !error) return null;
    return <section className="question-panel" aria-label={t("materials.pendingQuestions")}>
        {pending.map((question) => <QuestionCard key={question.id} question={question} refresh={load} onResolved={acknowledge} />)}
        {resolved.length > 0 && <details className="question-history"><summary>{t("materials.questionHistory", { count: resolved.length })}</summary><div className="question-history-items">
            {history.map((question) => <QuestionCard key={question.id} question={question} refresh={load} onResolved={acknowledge} />)}
            {resolved.length > history.length && <Button size="sm" color="link-gray" className="question-history-more" onClick={() => setDepth(depth + 20)}>{t("materials.questionHistoryMore", { count: resolved.length - history.length })}</Button>}
        </div></details>}
        {error && <div role="alert" className="question-error">{error}<Button size="sm" color="link-gray" onClick={() => void load()}>{t("materials.refresh")}</Button></div>}
    </section>;
}

interface AnswerDraft { choice: string; text: string; writing: boolean; deferred: boolean; pending?: QuestionAnswer }
const emptyDraft: AnswerDraft = { choice: "", text: "", writing: false, deferred: false };
function readDraft(key: string): AnswerDraft {
    try {
        const saved = JSON.parse(sessionStorage.getItem(key) || "null");
        if (!saved || typeof saved !== "object") return emptyDraft;
        const pending = saved.pending;
        const validPending = pending && typeof pending.command_id === "string" && ["accept", "decline", "cancel"].includes(pending.decision)
            && (pending.choice === undefined || typeof pending.choice === "string") && (pending.text === undefined || typeof pending.text === "string");
        return { choice: typeof saved.choice === "string" ? saved.choice : "", text: typeof saved.text === "string" ? saved.text : "", writing: saved.writing === true, deferred: saved.deferred === true, ...(validPending ? { pending } : {}) };
    } catch { return emptyDraft; }
}
function acknowledges(question: PendingQuestion | undefined, id: string, body: QuestionAnswer): question is PendingQuestion {
    return !!question && question.id === id && ["answered", "declined", "cancelled"].includes(question.state)
        && question.answer?.command_id === body.command_id && question.answer.decision === body.decision
        && (question.answer.choice || "") === (body.choice || "") && (question.answer.text || "") === (body.text || "");
}

function QuestionCard({ question, refresh, onResolved }: { question: PendingQuestion; refresh: () => Promise<void>; onResolved: (question: PendingQuestion) => void }) {
    const { t, locale } = useI18n();
    const nodeLabelOf = useNodeLabel();
    // Who is waiting reads as a person would say it — the agent and the
    // machine's name — because a thread can have several agents asking
    // from several machines and the requests look otherwise identical.
    const asker = whoIs(nodeLabelOf, question.agent, question.node);
    const key = `steve.question.answer:${question.id}`;
    const [draft, setDraft] = useState(() => readDraft(key));
    const [busy, setBusy] = useState(false);
    const acting = useRef(false);
    const textInput = useRef<HTMLTextAreaElement>(null);
    const choices = useRef<HTMLDivElement>(null);
    const [error, setError] = useState("");
    const titleId = useId();
    const active = question.state === "pending";
    const permission = question.kind === "permission";
    const freeText = !permission && question.allow_free_text === true;
    const pending = draft.pending;
    const disabled = busy || !!pending;
    const writing = freeText && (draft.writing || question.options.length === 0);
    const deferred = active && draft.deferred && !pending;
    const chosen = question.options.find((option) => option.id === question.answer?.choice);
    const response = question.answer?.text || chosen?.label || question.answer?.choice;

    function saveDraft(next: AnswerDraft) {
        setDraft(next);
        try { sessionStorage.setItem(key, JSON.stringify(next)); return true; }
        catch { setError(t("materials.answerStorage")); return false; }
    }
    function edit(patch: Partial<AnswerDraft>) { setError(""); saveDraft({ ...draft, ...patch }); }
    async function submit(decision: QuestionAnswer["decision"], reply: Pick<QuestionAnswer, "choice" | "text"> = {}, retry = false) {
        if (acting.current || !active || (!retry && pending)) return;
        if (!retry && decision === "accept" && !reply.choice && !reply.text?.trim()) {
            setError(t("materials.answerRequired"));
            if (writing) textInput.current?.focus();
            else choices.current?.querySelector<HTMLElement>('[role="radio"]')?.focus();
            return;
        }
        const body: QuestionAnswer = retry && pending ? pending : { command_id: `answer-${crypto.randomUUID()}`, decision, ...(decision === "accept" ? reply : {}) };
        if (!saveDraft({ ...draft, pending: body, deferred: false })) return;
        acting.current = true;
        setBusy(true); setError("");
        try {
            const result = await answerQuestion(question.id, body);
            if (!acknowledges(result.question, question.id, body)) throw new Error(t("materials.answerUnconfirmed"));
            try { sessionStorage.removeItem(key); } catch { /* The acknowledged response remains authoritative. */ }
            onResolved(result.question);
        } catch (error) {
            if (error instanceof HTTPError && error.status === 400) saveDraft({ ...draft, pending: undefined });
            setError(error instanceof Error ? error.message : String(error));
            await refresh();
        } finally { acting.current = false; setBusy(false); }
    }

    const kindLabel = t(permission ? "materials.permission" : active ? "materials.pending" : "materials.question");
    const title = question.title?.trim() || "";
    // A resolved request is a record, not a prompt: one line says what was
    // asked and what was answered; the original wording stays a click away.
    if (!active) return <article className="question-card question-done" aria-labelledby={titleId}>
        <div className="question-done-row">
            {title && <span className="question-kind">{kindLabel}</span>}
            <h3 id={titleId}>{title || kindLabel}</h3>
            {asker && <span className="question-done-who" title={question.node || undefined}>{asker}</span>}
            <span className="question-done-outcome"><span role="status">{t(`materials.${question.state}`)}</span>{response && <><span aria-hidden="true">·</span><span className="question-done-answer">{response}</span></>}</span>
            <time className="question-done-time" dateTime={question.updated_at}>{dateTime(question.updated_at, locale, { dateStyle: "short", timeStyle: "short" })}</time>
        </div>
        {question.message.trim() && <details className="question-context"><summary>{t("materials.questionContext")}</summary><Md text={question.message} className="question-message" /></details>}
    </article>;
    return <article className={`question-card ${permission ? "question-permission" : ""}`} aria-labelledby={titleId}>
        <header className="question-header">
            {title && <span className="question-kind">{kindLabel}</span>}
            <h3 id={titleId}>{title || kindLabel}</h3>
            {asker && <p className="question-asker" title={question.node || undefined}>{t("materials.askedBy", { who: asker })}</p>}
        </header>
        {!deferred && (active && writing && question.options.length > 0 ? <details className="question-context"><summary>{t("materials.questionContext")}</summary><Md text={question.message} className="question-message" /></details> : <Md text={question.message} className="question-message" />)}
        {deferred ? <div className="question-deferred">
            <p role="status">{t("materials.answerDeferred")}</p>
            <Button size="sm" color="secondary" onClick={() => edit({ deferred: false })}>{t("materials.replyNow")}</Button>
        </div> : <div className="question-response">
            {permission ? <RadioGroup ref={choices} aria-label={t("materials.chooseOption")} value={draft.choice} onChange={(choice) => edit({ choice })} isDisabled={disabled} className="question-permission-options">
                {question.options.map((option) => <Radio key={option.id} value={option.id} className="question-radio"><span className="question-radio-dot" aria-hidden="true" /><span className="question-option-copy"><span className="question-option-label">{option.label}</span>{option.description && <span className="question-option-description">{option.description}</span>}</span></Radio>)}
            </RadioGroup> : !writing && <div className="question-options">
                {question.options.map((option) => <Button key={option.id} size="md" color="secondary" aria-label={option.label} className="question-option" isDisabled={disabled} isLoading={busy && pending?.choice === option.id} showTextWhileLoading onClick={() => void submit("accept", { choice: option.id })}>
                    <span className="question-option-copy"><span className="question-option-label">{option.label}</span>{option.description && <span className="question-option-description">{option.description}</span>}</span>
                </Button>)}
            </div>}
            {writing && <form className="question-free-text" onSubmit={(event) => { event.preventDefault(); void submit("accept", { text: draft.text }); }}>
                <TextArea label={t("materials.answer")} name="question-answer" autoComplete="off" placeholder={t("materials.answerPlaceholder")} value={draft.text} onChange={(text) => edit({ text })} isDisabled={disabled} textAreaRef={textInput} rows={3} size="sm" hint={t("materials.answerTarget")} />
                {!pending && <div className="question-actions"><Button type="submit" size="sm" isLoading={busy}>{t("materials.accept")}</Button>{question.options.length > 0 && <Button size="sm" color="tertiary" onClick={() => edit({ writing: false })}>{t("materials.backToOptions")}</Button>}<Button size="sm" color="tertiary" onClick={() => edit({ deferred: true })}>{t("materials.answerLater")}</Button></div>}
            </form>}
            {error && <p role="alert" className="question-error">{error}</p>}
            {pending ? <div className="question-delivery">
                <p role="status">{t(busy ? "materials.answerPending" : "materials.answerUnconfirmed")}</p>
                {!busy && <><p className="question-pending-answer">{pending.text || question.options.find((option) => option.id === pending.choice)?.label || t(pending.decision === "decline" ? "materials.decline" : "materials.cancelQuestion")}</p><Button size="sm" onClick={() => void submit(pending.decision, {}, true)}>{t("materials.answerRetry")}</Button></>}
            </div> : (!writing || permission) && <div className="question-actions">
                {permission ? <><Button size="sm" isLoading={busy} onClick={() => void submit("accept", { choice: draft.choice })}>{t("materials.accept")}</Button><Button size="sm" color="secondary-destructive" isDisabled={busy} onClick={() => void submit("decline")}>{t("materials.decline")}</Button><Button size="sm" color="tertiary" isDisabled={busy} onClick={() => void submit("cancel")}>{t("materials.cancelQuestion")}</Button><ApprovalModeSwitch question={question} /></> : <>
                    {freeText && !writing && <Button size="sm" color="tertiary" onClick={() => edit({ writing: true })}>{t("materials.writeAnswer")}</Button>}
                    <Button size="sm" color="tertiary" onClick={() => edit({ deferred: true })}>{t("materials.answerLater")}</Button>
                </>}
            </div>}
        </div>}
        {question.kind !== "recovery" && question.deadline && !question.deadline.startsWith("0001-") && <p className="question-deadline">{t("materials.deadline", { time: dateTime(question.deadline, locale, { dateStyle: "short", timeStyle: "short" }) })}</p>}
    </article>;
}

// ApprovalModeSwitch is the way out of answering one request after
// another: the agent's own approval modes, offered where the approving
// happens. The choice is the same one the composer's chip makes, so it is
// remembered for the thread; a running session takes it immediately,
// which is the point — the flood of requests is happening now.
function ApprovalModeSwitch({ question }: { question: PendingQuestion }) {
    const { t } = useI18n();
    const [selectors, setSelectors] = useState<Selectors | null>(null);
    const [busy, setBusy] = useState(false);
    const [note, setNote] = useState("");
    const [error, setError] = useState("");
    const agent = question.agent || "";
    if (!agent) return null;
    const approval = selectors?.options?.find((option) => option.Category === "mode" || option.ID === "mode");
    const choices = approval?.Choices ?? [];
    const current = approval ? selectors?.preferred?.[approval.ID] || approval.Current : undefined;
    function open() {
        if (busy || selectors) return;
        setBusy(true); setError("");
        fetchSelectors(question.conversation, agent)
            .then(setSelectors)
            .catch((failure) => setError(String(failure).replace(/^Error: /, "")))
            .finally(() => setBusy(false));
    }
    async function choose(value: string) {
        if (busy || !approval) return;
        setBusy(true); setError(""); setNote("");
        try {
            const result = await setPreferences(question.conversation, agent, { [approval.ID]: value });
            const label = choices.find((choice) => choice.Value === value)?.Label || value;
            setNote(t(result.live ? "materials.modeNow" : "materials.modeNextTurn", { mode: label }));
            setSelectors(await fetchSelectors(question.conversation, agent));
        } catch (failure) {
            setError(String(failure).replace(/^Error: /, ""));
        } finally { setBusy(false); }
    }
    return <>
        <Dropdown.Root onOpenChange={(isOpen) => { if (isOpen) open(); }}>
            <AriaButton isDisabled={busy} className="question-mode-trigger">
                <span>{t("materials.stopAsking")}</span>
                <ChevronDown className="size-3" aria-hidden="true" />
            </AriaButton>
            <Dropdown.Popover placement="top start" className="w-72">
                {error && !selectors ? <div className="px-3 py-2 text-xs text-error-primary">{error}</div> : !selectors ? <div className="px-3 py-2 text-xs text-quaternary">{t("consoleChrome.loadingChoices")}</div> : (
                    <Dropdown.Menu onAction={(key) => void choose(String(key))}>
                        <Dropdown.Section>
                            <Dropdown.SectionHeader className="px-2 py-1 u-meta text-quaternary">{t("materials.modeHint", { agent })}</Dropdown.SectionHeader>
                            {choices.length === 0 && <Dropdown.Item id="__none" label={t("consoleChrome.noApprovalSelector")} isDisabled />}
                            {choices.map((choice) => <Dropdown.Item key={choice.Value} id={choice.Value} label={`${choice.Label || choice.Value}${choice.Value === current ? " · " + t("materials.modeCurrent") : ""}`} isDisabled={choice.Value === current} />)}
                        </Dropdown.Section>
                    </Dropdown.Menu>
                )}
            </Dropdown.Popover>
        </Dropdown.Root>
        {note && <span role="status" className="question-mode-note">{note}</span>}
        {error && selectors && <span role="alert" className="question-mode-note text-error-primary">{error}</span>}
    </>;
}
