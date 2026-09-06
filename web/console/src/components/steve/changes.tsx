import { useI18n } from "@/providers/locale-provider";
import { number } from "@/lib/format";
import { useEffect, useState } from "react";
import { ChevronDown, Loading01, Maximize01 } from "@untitledui/icons";
import { fetchAttemptChanges } from "@/lib/api";
import type { ChangeIndex, ChangeSummary } from "@/lib/types";
import { Button } from "@/components/base/buttons/button";
import { useReview } from "./review-context";

const statusTone: Record<string, string> = { A: "text-fg-success-primary", M: "text-fg-warning-primary", D: "text-fg-error-primary" };
const statusWord = { A: "consoleChrome.changeAdded", M: "consoleChrome.changeModified", D: "consoleChrome.changeDeleted", T: "consoleChrome.changeType" } as const;

export function ChangesFold({ summary, label }: { summary: ChangeSummary; label?: string }) {
    return <ChangesFoldContent key={summary.attempt} summary={summary} label={label} />;
}

function ChangesFoldContent({ summary, label }: { summary: ChangeSummary; label?: string }) {
    const { t, locale } = useI18n();
    const [open, setOpen] = useState(false);
    const [index, setIndex] = useState<ChangeIndex | null>(null);
    const [error, setError] = useState("");
    const [retry, setRetry] = useState(0);
    const review = useReview();
    useEffect(() => {
        if (!open || index) return;
        let gone = false;
        setError("");
        void fetchAttemptChanges(summary.attempt).then((value) => { if (!gone) setIndex(value); }).catch((e) => { if (!gone) setError(String(e).replace(/^Error: /, "")); });
        return () => { gone = true; };
    }, [open, index, summary.attempt, retry]);
    if (!summary.files && !summary.note) return null;
    const title = summary.files ? t("consoleChrome.changedFiles", { count: number(summary.files, locale) }) : t("consoleChrome.viewChanges");
    return (
        <details open={open} onToggle={(e) => setOpen(e.currentTarget.open)} className="group/changes min-w-0">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 py-0.5 text-xs text-tertiary hover:text-primary">
                <span>{title}</span>
                {label && <span className="text-quaternary">· {label}</span>}
                {summary.note && <span className="text-quaternary">· {summary.note}</span>}
                <ChevronDown aria-hidden="true" className="size-3.5 shrink-0 transition group-open/changes:rotate-180" />
            </summary>
            <div className="mt-2 flex min-w-0 flex-col gap-2 border-l border-secondary pl-3">
                <Button size="sm" color="secondary" iconLeading={Maximize01} className="self-start" onClick={() => review({ attempt: summary.attempt, label, index: index ?? undefined })}>{t("consoleChrome.reviewChanges")}</Button>
                {!index && !error && <span role="status" className="flex items-center gap-1.5 px-1.5 py-1 text-xs text-quaternary"><Loading01 aria-hidden="true" className="size-3 animate-spin" />{t("consoleChrome.loadingChanges")}</span>}
                {error && <div role="alert" className="text-xs text-error-primary">{error} <Button size="sm" color="link-gray" onClick={() => setRetry((n) => n + 1)}>{t("common.retry")}</Button></div>}
                {index && <ul className={`flex flex-col ${index.changes.length > 12 ? "max-h-80 overflow-y-auto" : ""}`}>
                    {index.changes.map((file) => <li key={file.path}><button type="button" aria-label={t("consoleChrome.reviewFile", { path: file.path })} onClick={() => review({ attempt: summary.attempt, path: file.path, label, index })} className="flex min-h-9 w-full items-center gap-2 rounded-md px-1.5 py-2 text-left text-xs hover:bg-secondary">
                        <span className={`w-5 shrink-0 font-mono ${statusTone[file.status] ?? "text-tertiary"}`} title={statusWord[file.status as keyof typeof statusWord] ? t(statusWord[file.status as keyof typeof statusWord]) : file.status}>{file.status}</span>
                        <span className="min-w-0 flex-1 truncate font-mono text-secondary" title={file.path}>{file.path}</span>
                        {file.binary ? <span className="text-quaternary">{t("consoleChrome.binary")}</span> : <span className="shrink-0 font-mono"><span className="text-fg-success-primary">+{number(file.added, locale)}</span> <span className="text-fg-error-primary">−{number(file.deleted, locale)}</span></span>}
                    </button></li>)}
                    {index.truncated && <li className="px-1.5 py-1 text-xs text-quaternary">{t("consoleChrome.firstItems", { count: number(index.changes.length, locale) })}</li>}
                </ul>}
            </div>
        </details>
    );
}
