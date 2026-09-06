import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { Check, Copy01 } from "@untitledui/icons";
import { useI18n } from "@/providers/locale-provider";
import { MaterialActions, type CaptureSpec } from "./material-actions";
import { MaterialPreview } from "./material-shelf";
import { captureMaterial } from "@/lib/api/material";
import { Button } from "@/components/base/buttons/button";
import { langName } from "@/components/steve/markdown";
import { sourceLanguage, sourceLines } from "@/lib/source-language";
import type { FileView } from "@/lib/types";
import "@/styles/source-view.css";

const pageSize = 1000;

export interface SourceReadingState {
    wrap: boolean;
    limit: number;
    scrollTop: number;
    scrollLeft: number;
}

interface SourceViewProps {
    file: FileView;
    capture?: CaptureSpec;
    lang?: string;
    readingState?: SourceReadingState;
    onReadingStateChange?: (state: SourceReadingState) => void;
}

export function SourceView(props: SourceViewProps) {
    const { file } = props;
    return <SourceContent key={`${file.attempt}:${file.commit}:${file.path}`} {...props} />;
}

function SourceContent({ file, lang, readingState, onReadingStateChange, capture }: SourceViewProps) {
    const { t } = useI18n();
    const [selection, setSelection] = useState<{ start: number; end: number } | null>(null);
    const [preview, setPreview] = useState<string | null>(null);
    const [captureError, setCaptureError] = useState("");
    const [capturing, setCapturing] = useState(false);
    const [wrap, setWrap] = useState(readingState?.wrap ?? false);
    const [limit, setLimit] = useState(readingState?.limit ?? pageSize);
    const scroll = useRef<HTMLDivElement>(null);
    const reading = useRef<SourceReadingState>(readingState ? { ...readingState } : { wrap: false, limit: pageSize, scrollTop: 0, scrollLeft: 0 });
    const onReadingChange = useRef(onReadingStateChange);
    onReadingChange.current = onReadingStateChange;
    const [copyState, setCopyState] = useState<"idle" | "copying" | "copied" | "failed">("idle");
    const copying = useRef(false);
    const language = sourceLanguage(file.path, lang);
    const rawLines = useMemo(() => file.text.split(/\r\n|\r|\n/), [file.text]);
    const visible = useMemo(() => sourceLines(rawLines.slice(0, limit).join("\n"), language), [rawLines, limit, language]);
    const total = file.text ? rawLines.length : 0;
    const remaining = Math.max(0, total - limit);
    const size = file.size < 1024 ? `${file.size} B` : `${(file.size / 1024).toFixed(1)} KB`;

    const selectedRange = selection ? { kind: "lines" as const, start: Math.min(selection.start, selection.end), end: Math.max(selection.start, selection.end) } : undefined;
    function selectLine(line: number, extend: boolean) { setSelection((old) => ({ start: extend && old ? old.start : line, end: line })); }
    async function previewCapture() { if (!capture || capturing) return; setCapturing(true); setCaptureError(""); try { const material = await captureMaterial(capture.project, capture.source, capture.title); setPreview(material.id); } catch (error) { setCaptureError(String(error)); } finally { setCapturing(false); } }
    function remember(patch: Partial<SourceReadingState> = {}) {
        const element = scroll.current;
        reading.current = {
            ...reading.current,
            ...(element ? { scrollTop: element.scrollTop, scrollLeft: element.scrollLeft } : {}),
            ...patch,
        };
        onReadingChange.current?.(reading.current);
    }

    useLayoutEffect(() => {
        const element = scroll.current;
        if (!element) return;
        element.scrollTop = reading.current.scrollTop;
        element.scrollLeft = reading.current.scrollLeft;
        return () => {
            // Capture the last position even if a tab change beats its scroll event.
            reading.current = { ...reading.current, scrollTop: element.scrollTop, scrollLeft: element.scrollLeft };
            onReadingChange.current?.(reading.current);
        };
    }, []);

    async function copy() {
        if (copying.current) return;
        copying.current = true;
        setCopyState("copying");
        try {
            if (!navigator.clipboard?.writeText) throw new Error("clipboard unavailable");
            await navigator.clipboard.writeText(file.text);
            setCopyState("copied");
        } catch {
            setCopyState("failed");
        } finally {
            copying.current = false;
        }
    }

    return (
        <section className="source-view" aria-label={t("console.sourceLabel", { path: file.path })}>
            <div className="source-toolbar">
                {capture && <MaterialActions capture={capture} selector={selectedRange} />}
                {capture && file.binary && <Button size="sm" color="secondary" isLoading={capturing} onClick={() => void previewCapture()}>{t("materials.preview")}</Button>}
                <div className="source-file-meta"><span>{file.binary ? t("console.binary") : langName(lang || (language === "ini" && file.path.endsWith(".toml") ? "toml" : language === "xml" && /\.html?$/i.test(file.path) ? "html" : language))}</span><span>{size}</span>{!file.binary && <span>{t("console.lineCount", { count: total })}{file.truncated ? t("console.retrieved") : ""}</span>}<span>{t("console.readOnly")}</span></div>
                {!file.binary && <div className="source-actions">
                    <Button size="xs" color="tertiary" aria-pressed={wrap} onClick={() => { remember({ wrap: !wrap }); setWrap(!wrap); }}>{t("console.wrap")}</Button>
                    <Button size="xs" color="tertiary" iconLeading={copyState === "copied" ? Check : Copy01} isLoading={copyState === "copying"} isDisabled={!file.text} onClick={() => void copy()}>{copyState === "copied" ? t("console.copied") : t("console.copy")}</Button>
                </div>}
            </div>
            <div className={`source-copy-status ${copyState === "failed" ? "" : "sr-only"}`} role="status" aria-live="polite">{copyState === "copied" ? (file.truncated ? t("console.copiedPartial") : t("console.copiedFile")) : copyState === "failed" ? t("console.copyFailed") : ""}</div>
            {selectedRange && <div role="status" className="flex items-center gap-3 px-3 py-2 text-xs"><span>{t("materials.selection", { start: selectedRange.start, end: selectedRange.end })}</span><button type="button" className="underline" onClick={() => setSelection(null)}>{t("materials.clearSelection")}</button></div>}
            {captureError && <p role="alert" className="text-sm text-error-primary">{captureError}</p>}
            {preview && capture && <MaterialPreview project={capture.project} anchor={{ id: preview }} onClose={() => setPreview(null)} />}
            {file.truncated && <p className="source-notice" role="status">{t("console.partialFile")}</p>}
            {file.binary ? <div className="source-empty"><p>{t("console.binaryFile")}</p><span>{t("console.binaryHint")}</span></div> : !file.text ? <div className="source-empty"><p>{t("console.emptyFile")}</p><span>{t("console.emptyFileHint")}</span></div> : (
                <div ref={scroll} onScroll={() => remember()} className={`source-scroll ${wrap ? "source-wrap" : ""}`} tabIndex={0} role="region" aria-label={t("console.fileContentLabel", { path: file.path })}>
                    <pre className="source-code"><code>{visible.map((html, index) => <span className="source-line" key={index}><button type="button" className={`source-number ${selectedRange && index + 1 >= selectedRange.start && index + 1 <= selectedRange.end ? "bg-brand-primary text-brand-secondary" : ""}`} data-line={index + 1} aria-label={t("materials.selectLine", { line: index + 1 })} aria-pressed={!!selectedRange && index + 1 >= selectedRange.start && index + 1 <= selectedRange.end} tabIndex={selection ? selection.end === index + 1 ? 0 : -1 : index === 0 ? 0 : -1} onClick={(event) => selectLine(index + 1, event.shiftKey)} onKeyDown={(event) => { const next = event.key === "ArrowDown" ? Math.min(index + 2, visible.length) : event.key === "ArrowUp" ? Math.max(index, 1) : null; if (next !== null) { event.preventDefault(); selectLine(next, event.shiftKey); scroll.current?.querySelector<HTMLButtonElement>(`[data-line="${next}"]`)?.focus(); } }}>{index + 1}</button><span className="source-text" dangerouslySetInnerHTML={{ __html: html || "\n" }} /></span>)}</code></pre>
                    {remaining > 0 && <div className="source-more"><span>{t("console.shownLines", { shown: Math.min(limit, total), total })}</span><Button size="sm" color="secondary" onClick={() => { remember({ limit: limit + pageSize }); setLimit(limit + pageSize); }}>{t("console.moreLines", { count: Math.min(pageSize, remaining) })}</Button></div>}
                </div>
            )}
        </section>
    );
}
