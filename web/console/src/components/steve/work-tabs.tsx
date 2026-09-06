import { useI18n } from "@/providers/locale-provider";
import { when, number } from "@/lib/format";
import type { Locale, Translator } from "@/lib/i18n";
import { useEffect, useMemo, useState } from "react";
import { Code02, File02 } from "@untitledui/icons";
import { fetchTaskAttempts } from "@/lib/api";
import type { AttemptView, Task } from "@/lib/types";
import { Button } from "@/components/base/buttons/button";
import { useReview } from "./review-context";
import { Nothing } from "./ui";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");

// useAttempts gathers the attempts of the thread's tasks and their
// children, newest first, refetched when the tasks move.
function useAttempts(roots: Task[], all: Task[]) {
    const ids = useMemo(() => {
        const out: string[] = [];
        const walk = (t: Task, depth: number) => {
            if (depth > 4 || out.includes(t.id)) return;
            out.push(t.id);
            all.filter((c) => c.parent === t.id).forEach((c) => walk(c, depth + 1));
        };
        roots.forEach((t) => walk(t, 0));
        return out;
    }, [roots, all]);
    const key = ids.join(",") + "|" + all.filter((t) => ids.includes(t.id)).map((t) => t.updated_at || "").join(",");
    const [attempts, setAttempts] = useState<(AttemptView & { task: string })[]>([]);
    const [error, setError] = useState("");
    const [loading, setLoading] = useState(true);
    const [retry, setRetry] = useState(0);
    useEffect(() => {
        let gone = false;
        setLoading(true); setError("");
        void Promise.all(ids.map((id) => fetchTaskAttempts(id).then((list) => list.map((a) => ({ ...a, task: id })))))
            .then((lists) => { if (gone) return; setAttempts(lists.flat().sort((a, b) => b.started_at.localeCompare(a.started_at))); setError(""); })
            .catch((e) => { if (!gone) setError(fail(e)); })
            .finally(() => { if (!gone) setLoading(false); });
        return () => { gone = true; };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [key, retry]);
    return { attempts, error, loading, retry: () => setRetry((n) => n + 1), tasks: ids };
}

function attemptLabel(a: AttemptView & { task: string }, tasks: Task[], tr: Translator, locale: Locale): string {
    const t = tasks.find((x) => x.id === a.task);
    const who = [a.agent, a.node].filter(Boolean).join(" @ ");
    return `#${a.task}${t?.parent ? tr("consoleChrome.delegatedSuffix") : ""} · ${who || a.kind} · ${when(a.started_at, locale)}`;
}

export function CodeTab({ roots, all }: { roots: Task[]; all: Task[] }) {
    const { t, locale } = useI18n();
    const { attempts, error, loading, retry } = useAttempts(roots, all);
    const open = useReview();
    const browsable = attempts.filter((attempt) => attempt.artifact || attempt.base);
    const choices = browsable.map((attempt) => ({ id: attempt.id, label: attemptLabel(attempt, all, t, locale), base: attempt.base, artifact: attempt.artifact }));
    if (error) return <div role="alert" className="text-sm text-error-primary">{error}<Button size="sm" color="secondary" onClick={retry}>{t("common.retry")}</Button></div>;
    if (loading && !attempts.length) return <p role="status" className="text-sm text-tertiary">{t("consoleChrome.loadingExecutions")}</p>;
    if (!browsable.length) return <Nothing icon={File02} title={t("consoleChrome.noSnapshots")}>{t("consoleChrome.noSnapshotsHint")}</Nothing>;
    return <section className="code-snapshots" aria-label={t("consoleChrome.snapshots")}>
        <div className="mb-4"><h2 className="text-sm font-semibold text-primary">{t("consoleChrome.codeWorkspace")}</h2><p className="mt-1 text-xs leading-relaxed text-tertiary">{t("consoleChrome.codeHint")}</p></div>
        <ul className="flex flex-col divide-y divide-secondary">{browsable.map((attempt, i) => <li key={attempt.id} className="flex min-w-0 flex-col gap-3 py-4 first:pt-0">
            <div className="flex min-w-0 items-start gap-2"><Code02 aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-fg-tertiary" /><div className="min-w-0"><p className="text-xs leading-5 text-secondary [overflow-wrap:anywhere]">{attemptLabel(attempt, all, t, locale)}</p><p className="mt-1 text-xs text-tertiary">{i === 0 ? `${t("consoleChrome.latest")} · ` : ""}{attempt.artifact ? t("consoleChrome.endSnapshot") : t("consoleChrome.startSnapshot")}{attempt.artifact === attempt.base ? ` · ${t("consoleChrome.noFileChanges")}` : attempt.files ? ` · ${t("consoleChrome.changeCount", { count: number(attempt.files, locale) })}` : ""}</p></div></div>
            <div className="flex flex-wrap gap-2"><Button size="sm" color={i === 0 ? "primary" : "secondary"} onClick={() => open({ attempt: attempt.id, label: attemptLabel(attempt, all, t, locale), attempts: choices, scope: "files" })}>{t("consoleChrome.browseFiles")}</Button>{attempt.artifact && attempt.artifact !== attempt.base && <Button size="sm" color="secondary" onClick={() => open({ attempt: attempt.id, label: attemptLabel(attempt, all, t, locale), attempts: choices, scope: "changes" })}>{t("consoleChrome.viewChanges")}</Button>}</div>
        </li>)}</ul>
    </section>;
}
