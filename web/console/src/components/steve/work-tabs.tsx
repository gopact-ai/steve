import { useI18n } from "@/providers/locale-provider";
import { when, number } from "@/lib/format";
import type { Locale, Translator } from "@/lib/i18n";
import { ChevronDown, File02 } from "@untitledui/icons";
import { fetchAttempts, type AttemptScope } from "@/lib/api/work";
import { useWorkPage } from "@/hooks/use-work-page";
import type { AttemptView, NativeAttempt, Task } from "@/lib/types";
import { Button } from "@/components/base/buttons/button";
import { useReview } from "./review-context";
import { Nothing } from "./ui";
import { useNodeLabel, whoIs } from "@/lib/node-name";

// Keep server ordering: nonzero EndedAt (otherwise StartedAt), then native ID.
const completionTime = (a: AttemptView) => a.ended_at && !a.ended_at.startsWith("0001-") ? a.ended_at : "";

function attemptLabel(a: NativeAttempt, tasks: Task[], tr: Translator, locale: Locale, nodeLabelOf: (id: string) => string): string {
    const t = tasks.find((x) => x.id === a.task_id);
    const who = whoIs(nodeLabelOf, a.agent, a.node);
    return `#${a.task_id}${t?.parent ? tr("consoleChrome.delegatedSuffix") : ""} · ${who || a.kind} · ${when(a.started_at, locale)}`;
}

export function ArtifactsTab({ scope, all }: { scope: AttemptScope; all: Task[] }) {
    const { t, locale } = useI18n();
    const nodeLabelOf = useNodeLabel();
    const page = useWorkPage(`files:${scope.task_id ? `task:${scope.task_id}` : `conversation:${scope.conversation}`}`, (cursor, signal) => fetchAttempts(scope, cursor, signal));
    const { items: attempts, error, loading } = page;
    const open = useReview();
    const browsable = attempts.filter((attempt) => attempt.artifact || attempt.base);
    // A completed read-only turn has a base but may have no new artifact.
    const latest = browsable.find((attempt) => completionTime(attempt) || attempt.artifact) || browsable[0];
    const choices = browsable.map((attempt) => ({ id: attempt.id, label: attemptLabel(attempt, all, t, locale, nodeLabelOf), base: attempt.base, artifact: attempt.artifact }));
    const review = (attempt: typeof browsable[number], scope: "files" | "changes") => open({ attempt: attempt.id, label: attemptLabel(attempt, all, t, locale, nodeLabelOf), attempts: choices, scope });
    return <section className="code-snapshots py-4" aria-label={t("consoleChrome.snapshots")}>
        <Button size="sm" color="link-gray" isDisabled={loading} onClick={page.refresh}>{t("workHistory.refresh")}</Button>
        {error && <div role="alert" className="text-sm text-error-primary">{page.stale ? t("workHistory.stale") : error}<Button size="sm" color="secondary" isDisabled={loading} onClick={page.stale ? page.refresh : page.retry}>{t(page.stale ? "workHistory.refresh" : "common.retry")}</Button></div>}
        {loading && !attempts.length ? <p role="status" className="text-sm text-tertiary">{t("consoleChrome.loadingExecutions")}</p>
                : !latest ? error ? null : <Nothing icon={File02} title={t("consoleChrome.noSnapshots")}>{t("consoleChrome.noSnapshotsHint")}</Nothing> : <>
                    <div className="mb-4"><h2 className="text-sm font-semibold text-primary">{t("consoleChrome.artifactWorkspace")}</h2><p className="mt-1 text-xs leading-relaxed text-tertiary">{t("consoleChrome.artifactHint")}</p></div>
                    <Button size="sm" color="primary" iconLeading={File02} onClick={() => review(latest, "files")}>{t("consoleChrome.browseFiles")}</Button>
                    <p className="mt-3 text-xs text-tertiary">{latest.artifact ? t("consoleChrome.endSnapshot") : t("consoleChrome.startSnapshot")}</p>
                    <p className="mt-1 text-xs leading-5 text-secondary [overflow-wrap:anywhere]">{attemptLabel(latest, all, t, locale, nodeLabelOf)}</p>
                    <details className="group/versions mt-5 border-t border-secondary pt-4">
                        <summary className="flex cursor-pointer list-none items-center gap-2 rounded text-xs font-medium text-secondary outline-focus-ring focus-visible:outline-2 focus-visible:outline-offset-2">
                            <ChevronDown aria-hidden="true" className="size-3.5 shrink-0 -rotate-90 group-open/versions:rotate-0" />
                            <span>{t("consoleChrome.versionHistory")}</span><span className="text-quaternary">{number(browsable.length, locale)}{page.hasMore ? "+" : ""}</span>
                        </summary>
                        <ul className="mt-2 flex flex-col divide-y divide-secondary">{browsable.map((attempt) => <li key={attempt.id} className="flex min-w-0 flex-col gap-1 py-3">
                            <p className="text-xs leading-5 text-secondary [overflow-wrap:anywhere]">{attemptLabel(attempt, all, t, locale, nodeLabelOf)}</p>
                            <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
                                <p className="text-xs text-tertiary">{attempt.artifact ? t("consoleChrome.endSnapshot") : t("consoleChrome.startSnapshot")}{attempt.artifact === attempt.base ? ` · ${t("consoleChrome.noFileChanges")}` : attempt.files_known && attempt.files ? ` · ${t("consoleChrome.changeCount", { count: `${attempt.files_truncated ? "≥" : ""}${number(attempt.files, locale)}` })}` : ""}</p>
                                {attempt.files_error && <span role="status" className="text-warning-primary">{attempt.files_error}</span>}
                                {attempt.artifact && attempt.artifact !== attempt.base && <Button size="sm" color="link-gray" onClick={() => review(attempt, "changes")}>{t("consoleChrome.viewChanges")}</Button>}
                            </div>
                        </li>)}</ul>
                    </details>
                </>}
        {page.hasMore && <Button className="mt-3" size="sm" color="secondary" isDisabled={loading || page.stale} onClick={page.more}>{t("workHistory.more")}</Button>}
    </section>;
}
