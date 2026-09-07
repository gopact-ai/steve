import { useEffect, useId, useRef, useState } from "react";
import { ChevronDown, File02, Loading01, Maximize01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { fetchAttemptChanges } from "@/lib/api";
import { number } from "@/lib/format";
import type { ChangeIndex, ChangeSummary } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";
import { useReview } from "./review-context";
import "@/styles/changes-card.css";

const previewFiles = 3;
const statusWord = { A: "consoleChrome.changeAdded", M: "consoleChrome.changeModified", D: "consoleChrome.changeDeleted", T: "consoleChrome.changeType" } as const;

export function ChangesFold({ summary, label }: { summary: ChangeSummary; label?: string }) {
    if (!summary.files && !summary.note) return null;
    return <ChangesCard key={`${summary.attempt}:${summary.base || ""}:${summary.artifact || ""}`} summary={summary} label={label} />;
}

function ChangesCard({ summary, label }: { summary: ChangeSummary; label?: string }) {
    const { t, locale } = useI18n();
    const [open, setOpen] = useState(true);
    const [expanded, setExpanded] = useState(false);
    const [seen, setSeen] = useState(false);
    const [index, setIndex] = useState<ChangeIndex | null>(null);
    const [error, setError] = useState("");
    const [retry, setRetry] = useState(0);
    const card = useRef<HTMLElement>(null);
    const contentID = useId();
    const review = useReview();
    const canRead = !!summary.attempt && summary.files > 0;

    useEffect(() => {
        if (!canRead || seen || !card.current || typeof IntersectionObserver === "undefined") return;
        const observer = new IntersectionObserver((entries) => {
            if (entries.some((entry) => entry.isIntersecting)) {
                setSeen(true);
                observer.disconnect();
            }
        });
        observer.observe(card.current);
        return () => observer.disconnect();
    }, [canRead, seen]);

    useEffect(() => {
        if (!canRead || !seen) return;
        const controller = new AbortController();
        setError("");
        void fetchAttemptChanges(summary.attempt, controller.signal).then((value) => {
            if (controller.signal.aborted) return;
            if (value.attempt !== summary.attempt || (summary.base && summary.base !== value.base) || (summary.artifact && summary.artifact !== value.artifact)) {
                setError("snapshot-changed");
                return;
            }
            setIndex({ ...value, changes: value.changes || [] });
        }).catch((cause) => {
            if (!controller.signal.aborted) setError(String(cause).replace(/^Error: /, ""));
        });
        return () => controller.abort();
    }, [canRead, seen, summary.attempt, summary.base, summary.artifact, retry]);

    const files = index?.changes || [];
    const count = index ? files.length : summary.files;
    const partial = index ? !!index.truncated : !!summary.truncated;
    const textFiles = files.filter((file) => !file.binary);
    const totals = index
        ? textFiles.length ? textFiles.reduce((sum, file) => ({ added: sum.added + file.added, deleted: sum.deleted + file.deleted }), { added: 0, deleted: 0 }) : null
        : summary.added !== undefined && summary.deleted !== undefined && summary.binary_files !== summary.files ? { added: summary.added, deleted: summary.deleted } : null;
    const title = count ? t("consoleChrome.changedFiles", { count: `${number(count, locale)}${partial ? "+" : ""}` }) : t("consoleChrome.viewChanges");
    const visibleFiles = expanded ? files : files.slice(0, previewFiles);
    const notes = [...new Set([summary.note, index?.note].filter(Boolean))];
    const openReview = (path?: string) => review({
        attempt: summary.attempt, path, label, index: index ?? undefined, scope: "changes",
        attempts: [{ id: summary.attempt, label: label || t("console.attemptLabel", { id: summary.attempt.slice(0, 12) }), base: summary.base || index?.base, artifact: summary.artifact || index?.artifact }],
    });

    return <section ref={card} className="changes-card" aria-label={t("consoleChrome.changesCard")} data-attempt={summary.attempt}>
        <div className="changes-card-header">
            <button type="button" className="changes-card-toggle" aria-expanded={open} aria-controls={contentID} onClick={() => { setOpen((value) => !value); setSeen(true); }}>
                <ChevronDown aria-hidden="true" className="changes-card-chevron" data-open={open} />
                <span>{title}</span>
            </button>
            {totals && <span className="changes-card-totals" aria-label={t(partial ? "consoleChrome.partialLineTotals" : "consoleChrome.lineTotals", { added: number(totals.added, locale), deleted: number(totals.deleted, locale) })}>
                <span className="changes-added">+{number(totals.added, locale)}</span><span className="changes-deleted">−{number(totals.deleted, locale)}</span>
                {partial && <span className="changes-card-partial">{t("consoleChrome.partialTotals")}</span>}
            </span>}
            {canRead && <Button size="sm" color="tertiary" iconLeading={Maximize01} className="changes-card-review" onClick={() => openReview()}>{t("consoleChrome.reviewChanges")}</Button>}
        </div>
        <div id={contentID} hidden={!open} className="changes-card-body">
            {label && <p className="changes-card-label">{label}</p>}
            {notes.map((note) => <p className="changes-card-note" key={note}>{note}</p>)}
            {canRead && !index && !error && <div className="changes-card-loading" role={seen ? "status" : undefined}>
                <Loading01 aria-hidden="true" className="size-3.5 animate-spin motion-reduce:animate-none" />{t("consoleChrome.loadingChanges")}
                {!seen && typeof IntersectionObserver === "undefined" && <Button size="sm" color="link-gray" onClick={() => setSeen(true)}>{t("consoleChrome.viewChanges")}</Button>}
            </div>}
            {error && <div className="changes-card-error" role="alert"><span>{error === "snapshot-changed" ? t("consoleChrome.snapshotChanged") : error}</span><Button size="sm" color="link-gray" onClick={() => setRetry((value) => value + 1)}>{t("common.retry")}</Button></div>}
            {index && (files.length ? <ul className="changes-card-files" data-expanded={expanded}>
                {visibleFiles.map((file) => <li key={file.path}>
                    <button type="button" className="changes-card-file" aria-label={t("consoleChrome.reviewFile", { path: file.path })} onClick={() => openReview(file.path)}>
                        <File02 aria-hidden="true" className="changes-card-file-icon" />
                        <span className="changes-card-path" title={file.path} translate="no">{file.path}</span>
                        <span className="changes-card-status" data-status={file.status} aria-label={statusWord[file.status as keyof typeof statusWord] ? t(statusWord[file.status as keyof typeof statusWord]) : file.status}>{file.status}</span>
                        {file.binary ? <span className="changes-card-binary">{t("consoleChrome.binary")}</span> : <span className="changes-card-file-totals"><span className="changes-added">+{number(file.added, locale)}</span><span className="changes-deleted">−{number(file.deleted, locale)}</span></span>}
                    </button>
                </li>)}
            </ul> : <p className="changes-card-label">{t("consoleChrome.noFileChanges")}</p>)}
            {files.length > previewFiles && <button type="button" className="changes-card-more" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>
                {expanded ? t("consoleChrome.showFewerFiles") : t("consoleChrome.showMoreFiles", { count: number(files.length - previewFiles, locale) })}<ChevronDown aria-hidden="true" className="changes-card-chevron" data-open={expanded} />
            </button>}
            {index?.truncated && <p className="changes-card-note">{t("consoleChrome.firstItems", { count: number(files.length, locale) })}</p>}
        </div>
    </section>;
}
