import { useI18n } from "@/providers/locale-provider";
import { when, number } from "@/lib/format";
import type { Locale, Translator } from "@/lib/i18n";
import { useEffect, useMemo, useState } from "react";
import { ChevronDown, File02 } from "@untitledui/icons";
import { fetchTaskAttempts } from "@/lib/api";
import type { AttemptView, Task } from "@/lib/types";
import { Button } from "@/components/base/buttons/button";
import { useReview } from "./review-context";
import { Nothing } from "./ui";
import { useNodeLabel, whoIs } from "@/lib/node-name";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");
// Go's zero time can be serialized for attempts that have not ended.
const completionTime = (a: AttemptView) => a.ended_at && !a.ended_at.startsWith("0001-") ? a.ended_at : "";

// useAttempts gathers the attempts of the thread's tasks and their
// children, newest first, refetched when the tasks move.
function useAttempts(roots: Task[], all: Task[]) {
    const ids = useMemo(() => {
        const out: string[] = [], seen = new Set<string>(), pending = [...roots];
        const children = new Map<string, Task[]>();
        for (const task of all) if (task.parent) children.set(task.parent, [...(children.get(task.parent) || []), task]);
        while (pending.length) {
            const task = pending.pop()!;
            if (seen.has(task.id)) continue;
            seen.add(task.id); out.push(task.id);
            pending.push(...(children.get(task.id) || []));
        }
        return out;
    }, [roots, all]);
    const key = ids.join(",") + "|" + all.filter((t) => ids.includes(t.id)).map((t) => t.updated_at || "").join(",");
    const [result, setResult] = useState<{ key: string; attempts: (AttemptView & { task: string })[]; error: string; loading: boolean }>({ key: "", attempts: [], error: "", loading: true });
    const [retry, setRetry] = useState(0);
    useEffect(() => {
        let gone = false;
        setResult((current) => ({ key, attempts: current.key === key ? current.attempts : [], error: "", loading: true }));
        void Promise.all(ids.map((id) => fetchTaskAttempts(id).then((list) => list.map((a) => ({ ...a, task: id })))))
            .then((lists) => { if (!gone) setResult({ key, attempts: lists.flat().sort((a, b) => Date.parse(completionTime(b) || b.started_at) - Date.parse(completionTime(a) || a.started_at)), error: "", loading: false }); })
            .catch((e) => { if (!gone) setResult({ key, attempts: [], error: fail(e), loading: false }); });
        return () => { gone = true; };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [key, retry]);
    // Never expose the previous conversation's files while the new scope loads.
    return { ...(result.key === key ? result : { attempts: [], error: "", loading: true }), retry: () => setRetry((n) => n + 1) };
}

function attemptLabel(a: AttemptView & { task: string }, tasks: Task[], tr: Translator, locale: Locale, nodeLabelOf: (id: string) => string): string {
    const t = tasks.find((x) => x.id === a.task);
    const who = whoIs(nodeLabelOf, a.agent, a.node);
    return `#${a.task}${t?.parent ? tr("consoleChrome.delegatedSuffix") : ""} · ${who || a.kind} · ${when(a.started_at, locale)}`;
}

export function ArtifactsTab({ roots, all }: { roots: Task[]; all: Task[] }) {
    const { t, locale } = useI18n();
    const nodeLabelOf = useNodeLabel();
    const { attempts, error, loading, retry } = useAttempts(roots, all);
    const open = useReview();
    const browsable = attempts.filter((attempt) => attempt.artifact || attempt.base);
    // A completed read-only turn has a base but may have no new artifact.
    const latest = browsable.find((attempt) => completionTime(attempt) || attempt.artifact) || browsable[0];
    const choices = browsable.map((attempt) => ({ id: attempt.id, label: attemptLabel(attempt, all, t, locale, nodeLabelOf), base: attempt.base, artifact: attempt.artifact }));
    const review = (attempt: typeof browsable[number], scope: "files" | "changes") => open({ attempt: attempt.id, label: attemptLabel(attempt, all, t, locale, nodeLabelOf), attempts: choices, scope });
    return <section className="code-snapshots py-4" aria-label={t("consoleChrome.snapshots")}>
        {error ? <div role="alert" className="text-sm text-error-primary">{error}<Button size="sm" color="secondary" onClick={retry}>{t("common.retry")}</Button></div>
            : loading && !attempts.length ? <p role="status" className="text-sm text-tertiary">{t("consoleChrome.loadingExecutions")}</p>
                : !latest ? <Nothing icon={File02} title={t("consoleChrome.noSnapshots")}>{t("consoleChrome.noSnapshotsHint")}</Nothing> : <>
                    <div className="mb-4"><h2 className="text-sm font-semibold text-primary">{t("consoleChrome.artifactWorkspace")}</h2><p className="mt-1 text-xs leading-relaxed text-tertiary">{t("consoleChrome.artifactHint")}</p></div>
                    <Button size="sm" color="primary" iconLeading={File02} onClick={() => review(latest, "files")}>{t("consoleChrome.browseFiles")}</Button>
                    <p className="mt-3 text-xs text-tertiary">{latest.artifact ? t("consoleChrome.endSnapshot") : t("consoleChrome.startSnapshot")}</p>
                    <p className="mt-1 text-xs leading-5 text-secondary [overflow-wrap:anywhere]">{attemptLabel(latest, all, t, locale, nodeLabelOf)}</p>
                    <details className="group/versions mt-5 border-t border-secondary pt-4">
                        <summary className="flex cursor-pointer list-none items-center gap-2 rounded text-xs font-medium text-secondary outline-focus-ring focus-visible:outline-2 focus-visible:outline-offset-2">
                            <ChevronDown aria-hidden="true" className="size-3.5 shrink-0 -rotate-90 group-open/versions:rotate-0" />
                            <span>{t("consoleChrome.versionHistory")}</span><span className="text-quaternary">{number(browsable.length, locale)}</span>
                        </summary>
                        <ul className="mt-2 flex flex-col divide-y divide-secondary">{browsable.map((attempt) => <li key={attempt.id} className="flex min-w-0 flex-col gap-1 py-3">
                            <p className="text-xs leading-5 text-secondary [overflow-wrap:anywhere]">{attemptLabel(attempt, all, t, locale, nodeLabelOf)}</p>
                            <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
                                <p className="text-xs text-tertiary">{attempt.artifact ? t("consoleChrome.endSnapshot") : t("consoleChrome.startSnapshot")}{attempt.artifact === attempt.base ? ` · ${t("consoleChrome.noFileChanges")}` : attempt.files ? ` · ${t("consoleChrome.changeCount", { count: number(attempt.files, locale) })}` : ""}</p>
                                {attempt.artifact && attempt.artifact !== attempt.base && <Button size="sm" color="link-gray" onClick={() => review(attempt, "changes")}>{t("consoleChrome.viewChanges")}</Button>}
                            </div>
                        </li>)}</ul>
                    </details>
                </>}
    </section>;
}
