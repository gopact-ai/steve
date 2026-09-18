import { useState } from "react";
import { useI18n } from "@/providers/locale-provider";
import type { FileView } from "@/lib/types";
import { Md, MarkdownDocument } from "./markdown";
import { Mermaid } from "./mermaid";
import "@/styles/file-preview.css";

// A produced file is usually meant to be read, not audited: a report in
// Markdown, a page in HTML, a diagram in Mermaid. These render as what
// they are, with the source one click away in the reading-mode switch.
export type PreviewKind = "markdown" | "html" | "svg" | "mermaid";

export function previewKind(path: string): PreviewKind | null {
    const name = path.toLowerCase();
    const ext = name.slice(name.lastIndexOf(".") + 1);
    switch (ext) {
        case "md": case "markdown": case "mdown": case "mkd": return "markdown";
        case "html": case "htm": return "html";
        case "svg": return "svg";
        case "mmd": case "mermaid": return "mermaid";
        default: return null;
    }
}

// Prose has a comfortable measure, but a reading pane that ignores the
// window is its own annoyance: a wide screen stays mostly empty and a
// diagram has nowhere to grow. The text follows the pane between bounds,
// and the reader picks the bound — kept for the next file read.
export type PreviewWidth = "narrow" | "comfortable" | "full";
const widthKey = "steve.preview.width";
const widths: PreviewWidth[] = ["narrow", "comfortable", "full"];
const widthLabels = { narrow: "console.previewNarrow", comfortable: "console.previewComfortable", full: "console.previewFull" } as const;

function savedWidth(): PreviewWidth {
    try { const stored = localStorage.getItem(widthKey) as PreviewWidth | null; return stored && widths.includes(stored) ? stored : "comfortable"; } catch { return "comfortable"; }
}

export function FilePreview({ file, kind, onOpenPath }: { file: FileView; kind: PreviewKind; onOpenPath?: (path: string) => void }) {
    const { t } = useI18n();
    const [width, setWidth] = useState(savedWidth);
    function chooseWidth(next: PreviewWidth) {
        setWidth(next);
        try { localStorage.setItem(widthKey, next); } catch { /* The current reading keeps the choice. */ }
    }
    if (file.binary) return <div className="file-preview-empty">{t("console.binaryFile")}</div>;
    if (!file.text.trim()) return <div className="file-preview-empty">{t("console.emptyFile")}</div>;
    const measured = kind === "markdown" || kind === "mermaid";
    return (
        <div className="file-preview" data-width={width} aria-label={t("console.previewLabel", { path: file.path })}>
            {file.truncated && <p role="status" className="file-preview-notice">{t("console.previewPartial")}</p>}
            {measured && <div className="file-preview-toolbar"><span className="workbench-segmented" role="group" aria-label={t("console.previewWidth")}>
                {widths.map((option) => <button key={option} type="button" aria-pressed={width === option} onClick={() => chooseWidth(option)}>{t(widthLabels[option])}</button>)}
            </span></div>}
            {kind === "markdown" && <div className="file-preview-prose">{onOpenPath
                ? <MarkdownDocument base={file.path} open={onOpenPath}><Md text={file.text} /></MarkdownDocument>
                : <Md text={file.text} />}</div>}
            {kind === "mermaid" && <div className="file-preview-prose"><Mermaid code={file.text} /></div>}
            {(kind === "html" || kind === "svg") && <Sandboxed file={file} kind={kind} />}
        </div>
    );
}

// A file is not trusted content: an artifact can be anything a run wrote.
// It is shown in a sandboxed frame with no access to this page, no
// same-origin privileges, and — for SVG, which needs none — no scripts.
function Sandboxed({ file, kind }: { file: FileView; kind: PreviewKind }) {
    const { t } = useI18n();
    return (
        <iframe
            title={t("console.previewLabel", { path: file.path })}
            className="file-preview-frame"
            sandbox={kind === "html" ? "allow-scripts" : ""}
            referrerPolicy="no-referrer"
            srcDoc={file.text}
        />
    );
}
