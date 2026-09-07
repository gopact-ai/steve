import { useEffect, useRef, useState } from "react";
import { Radio, RadioGroup } from "react-aria-components";
import { Button } from "@/components/base/buttons/button";
import { TextArea } from "@/components/base/textarea/textarea";
import { questions, answerQuestion } from "@/lib/api/material";
import type { PendingQuestion, QuestionAnswer } from "@/lib/types";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import { HTTPError } from "@/lib/http";
import { when } from "@/lib/format";

export function QuestionPanel({ conversation }: { conversation: string }) {
    const { t } = useI18n(); const { consoleEvents, live } = useFleet(); const [items, setItems] = useState<PendingQuestion[]>([]); const [error, setError] = useState("");
    const load = useResourceRead(`questions:${conversation}`, (signal) => questions(conversation, signal), (value) => { setItems((value.questions || []).map((question) => ({ ...question, options: question.options || [] }))); setError(""); }, (error) => setError(String(error)));
    const latest = consoleEvents.findLast((event) => event.kind === "console.question" && event.conversation === conversation)?.n;
    useEffect(() => { void load(); const timer = window.setInterval(() => void load(), 5000); return () => window.clearInterval(timer); }, [conversation, live, load]);
    useEffect(() => { void load(); }, [latest, load]);
    const pending = items.filter((question) => question.state === "pending").sort((a, b) => a.created_at.localeCompare(b.created_at));
    const history = items.filter((question) => question.state !== "pending").sort((a, b) => b.updated_at.localeCompare(a.updated_at)).slice(0, 5);
    if (pending.length === 0 && history.length === 0 && !error) return null;
    return <section className="mx-auto max-h-[40dvh] max-w-3xl space-y-3 overflow-y-auto overscroll-contain" aria-label={t("materials.pendingQuestions")}>
        {pending.map((question) => <QuestionCard key={question.id} question={question} refresh={load} />)}
        {history.length > 0 && <details className="rounded-lg border border-secondary p-3"><summary className="cursor-pointer text-xs text-tertiary">{t("materials.questionHistory", { count: history.length })}</summary><div className="mt-3 space-y-3">{history.map((question) => <QuestionCard key={question.id} question={question} refresh={load} />)}</div></details>}
        {error && <div role="alert" className="rounded-lg bg-error-primary p-3 text-sm text-error-primary">{error}<Button size="sm" color="link-gray" onClick={() => void load()}>{t("materials.refresh")}</Button></div>}
    </section>;
}
function QuestionCard({ question, refresh }: { question: PendingQuestion; refresh: () => Promise<void> }) {
    const { t, locale } = useI18n(); const key = `steve.question.answer:${question.id}`;
    const [saved] = useState(() => { try { return JSON.parse(sessionStorage.getItem(key) || "null") as { choice?: string; pending?: QuestionAnswer } | null; } catch { return null; } });
    const [choice, setChoice] = useState(saved?.choice || ""); const [answer, setAnswer] = useState<QuestionAnswer | null>(saved?.pending || null); const [busy, setBusy] = useState(false); const acting = useRef(false); const [error, setError] = useState("");
    const active = question.state === "pending";
    function change(value: string) { setChoice(value); try { sessionStorage.setItem(key, JSON.stringify({ choice: value })); } catch { /* The visible draft remains available. */ } }
    async function submit(decision: QuestionAnswer["decision"], retry = false) {
        if (acting.current || !active) return;
        if (!retry && answer) return;
        if (!retry && decision === "accept" && question.required && !choice.trim()) { setError(t("materials.answerRequired")); return; }
        const body = retry && answer ? answer : { command_id: `answer-${Date.now()}-${Math.random().toString(36).slice(2)}`, decision, ...(decision === "accept" && choice ? { choice } : {}) };
        try { sessionStorage.setItem(key, JSON.stringify({ choice, pending: body })); } catch { setError(t("materials.sourceUnavailable")); return; }
        acting.current = true; setBusy(true); setAnswer(body); setError("");
        try { await answerQuestion(question.id, body); sessionStorage.removeItem(key); setAnswer(null); await refresh(); }
        catch (error) { if (error instanceof HTTPError && error.status === 400) { setAnswer(null); try { sessionStorage.setItem(key, JSON.stringify({ choice })); } catch { /* Preserve the visible answer. */ } } setError(error instanceof Error ? error.message : String(error)); await refresh(); }
        finally { acting.current = false; setBusy(false); }
    }
    return <article className="rounded-lg border border-warning-secondary bg-warning-primary p-4 text-sm"><header className="mb-2 flex flex-wrap items-center justify-between gap-2"><h3 className="font-semibold">{question.title || t(question.kind === "permission" ? "materials.permission" : "materials.question")}</h3><span className="text-xs text-tertiary">{t("materials.deadline", { time: when(question.deadline, locale) })}</span></header><p className="whitespace-pre-wrap break-words">{question.message}</p>
        {active ? <div className="mt-3 space-y-3">{question.options.length ? <RadioGroup aria-label={t("materials.chooseOption")} value={choice} onChange={change} isDisabled={busy || !!answer} className="flex flex-col gap-2">{question.options.map((option) => <Radio key={option.id} value={option.id} className="group flex cursor-pointer gap-2 rounded-md border border-secondary bg-primary p-3 data-selected:ring-2 data-selected:ring-brand data-focus-visible:outline-2 data-focus-visible:outline-focus-ring"><span aria-hidden="true" className="mt-1 size-3 rounded-full border border-primary bg-secondary group-data-selected:bg-brand-solid" /><span><span className="font-medium">{option.label}</span>{option.description && <span className="mt-1 block text-xs text-tertiary">{option.description}</span>}</span></Radio>)}</RadioGroup> : <TextArea label={t("materials.answer")} placeholder={t("materials.answerPlaceholder")} value={choice} onChange={change} isDisabled={busy || !!answer} rows={3} />}
            {error && <p role="alert" className="text-error-primary">{error}</p>}
            <div className="flex flex-wrap gap-2">{answer ? <Button size="sm" isLoading={busy} onClick={() => void submit(answer.decision, true)}>{t("materials.answerRetry")}</Button> : <><Button size="sm" isLoading={busy} onClick={() => void submit("accept")}>{t("materials.accept")}</Button><Button size="sm" color="secondary-destructive" isDisabled={busy} onClick={() => void submit("decline")}>{t("materials.decline")}</Button><Button size="sm" color="secondary" isDisabled={busy} onClick={() => void submit("cancel")}>{t("materials.cancelQuestion")}</Button></>}</div>
        </div> : <p role="status" className="mt-3 text-tertiary">{t(`materials.${question.state}`)}</p>}
    </article>;
}
