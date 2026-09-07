import { useEffect, useId, useRef, useState } from "react";
import { Radio, RadioGroup } from "react-aria-components";
import { Button } from "@/components/base/buttons/button";
import { TextArea } from "@/components/base/textarea/textarea";
import { Md } from "@/components/steve/markdown";
import { questions, answerQuestion } from "@/lib/api/material";
import type { PendingQuestion, QuestionAnswer } from "@/lib/types";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import { HTTPError } from "@/lib/http";
import { dateTime } from "@/lib/format";
import "@/styles/questions.css";

export function QuestionPanel({ conversation }: { conversation: string }) {
    const { t } = useI18n();
    const { consoleEvents, live } = useFleet();
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
    const latest = consoleEvents.findLast((event) => event.kind === "console.question" && event.conversation === conversation)?.n;
    useEffect(() => { void load(); const timer = window.setInterval(() => void load(), 5000); return () => window.clearInterval(timer); }, [conversation, live, load]);
    useEffect(() => { void load(); }, [latest, load]);
    const pending = items.filter((question) => question.state === "pending").sort((a, b) => a.created_at.localeCompare(b.created_at));
    const history = items.filter((question) => question.state !== "pending").sort((a, b) => b.updated_at.localeCompare(a.updated_at)).slice(0, 5);
    function resolved(question: PendingQuestion) {
        setItems((previous) => previous.map((item) => item.id === question.id ? { ...question, options: question.options || [] } : item));
        void load();
    }
    if (pending.length === 0 && history.length === 0 && !error) return null;
    return <section className="question-panel" aria-label={t("materials.pendingQuestions")}>
        {pending.map((question) => <QuestionCard key={question.id} question={question} refresh={load} onResolved={resolved} />)}
        {history.length > 0 && <details className="question-history"><summary>{t("materials.questionHistory", { count: history.length })}</summary><div className="question-history-items">{history.map((question) => <QuestionCard key={question.id} question={question} refresh={load} onResolved={resolved} />)}</div></details>}
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

    return <article className={`question-card ${permission ? "question-permission" : ""}`} aria-labelledby={titleId}>
        <header className="question-header">
            <span className="question-kind">{t(permission ? "materials.permission" : active ? "materials.pending" : "materials.question")}</span>
            <h3 id={titleId}>{question.title || t(permission ? "materials.permission" : "materials.question")}</h3>
        </header>
        {!deferred && (active && writing && question.options.length > 0 ? <details className="question-context"><summary>{t("materials.questionContext")}</summary><Md text={question.message} className="question-message" /></details> : <Md text={question.message} className="question-message" />)}
        {active ? deferred ? <div className="question-deferred">
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
                {permission ? <><Button size="sm" isLoading={busy} onClick={() => void submit("accept", { choice: draft.choice })}>{t("materials.accept")}</Button><Button size="sm" color="secondary-destructive" isDisabled={busy} onClick={() => void submit("decline")}>{t("materials.decline")}</Button><Button size="sm" color="tertiary" isDisabled={busy} onClick={() => void submit("cancel")}>{t("materials.cancelQuestion")}</Button></> : <>
                    {freeText && !writing && <Button size="sm" color="tertiary" onClick={() => edit({ writing: true })}>{t("materials.writeAnswer")}</Button>}
                    <Button size="sm" color="tertiary" onClick={() => edit({ deferred: true })}>{t("materials.answerLater")}</Button>
                </>}
            </div>}
        </div> : <div className="question-resolved"><p role="status">{t(`materials.${question.state}`)}</p>{response && <div className="question-saved-answer"><span>{t("materials.yourAnswer")}</span><p>{response}</p></div>}</div>}
        {active && question.kind !== "recovery" && question.deadline && !question.deadline.startsWith("0001-") && <p className="question-deadline">{t("materials.deadline", { time: dateTime(question.deadline, locale, { dateStyle: "short", timeStyle: "short" }) })}</p>}
    </article>;
}
