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

export function FilePreview({ file, kind, onOpenPath }: { file: FileView; kind: PreviewKind; onOpenPath?: (path: string) => void }) {
    const { t } = useI18n();
    if (file.binary) return <div className="file-preview-empty">{t("console.binaryFile")}</div>;
    if (!file.text.trim()) return <div className="file-preview-empty">{t("console.emptyFile")}</div>;
    return (
        <div className="file-preview" aria-label={t("console.previewLabel", { path: file.path })}>
            {file.truncated && <p role="status" className="file-preview-notice">{t("console.previewPartial")}</p>}
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
