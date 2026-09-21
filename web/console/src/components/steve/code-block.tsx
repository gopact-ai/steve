import { useState, type ReactNode } from "react";
import { Check, Code02, Copy01, Terminal } from "@untitledui/icons";

// A block of code is shown the same way wherever it appears — a
// transcript, a tool call, a file, a diagram's source — so it lives on
// its own and no one has to reach through markdown for it.
const names: Record<string, string> = {
    sh: "Shell", bash: "Shell", shell: "Shell", zsh: "Shell", console: "Shell",
    json: "JSON", go: "Go", ts: "TypeScript", typescript: "TypeScript", tsx: "TSX", js: "JavaScript", javascript: "JavaScript", jsx: "JSX",
    py: "Python", python: "Python", rs: "Rust", rust: "Rust", yaml: "YAML", yml: "YAML", toml: "TOML", md: "Markdown", markdown: "Markdown",
    html: "HTML", css: "CSS", sql: "SQL", diff: "Diff", text: "文本", txt: "文本", plain: "文本",
};

export function langName(lang?: string): string {
    if (!lang) return "文本";
    return names[lang.toLowerCase()] ?? lang;
}

// CodeBlock is one block of code or output: a header naming what it is
// (and, for a command, what it came back with), a copy button, and the
// text itself, scrolling past a height.
export function CodeBlock({ code, lang, label, meta, muted, maxHeight = 320, density = "compact", className, sourceStart, sourceEnd }: { code: string; lang?: string; label?: ReactNode; meta?: ReactNode; muted?: boolean; maxHeight?: number; density?: "compact" | "reading"; className?: string; sourceStart?: number; sourceEnd?: number }) {
    const [copied, setCopied] = useState(false);
    const shell = lang === "shell" || lang === "sh" || lang === "bash" || lang === "console";
    const Icon = shell ? Terminal : Code02;
    function copy() {
        void navigator.clipboard?.writeText(code).then(() => { setCopied(true); window.setTimeout(() => setCopied(false), 1500); }).catch(() => undefined);
    }
    return (
        <div className={`not-prose ${density === "reading" ? "my-5" : "my-2"} flex min-w-0 flex-col overflow-hidden rounded-lg bg-secondary ring-1 ring-secondary ${className ?? ""}`}>
            <div data-selection-ignore className="flex items-center gap-1.5 border-b border-secondary px-3 py-1 u-meta text-quaternary">
                <Icon className="size-3.5 shrink-0" />
                <span className="font-medium">{label ?? langName(lang)}</span>
                {meta && <span className="truncate">· {meta}</span>}
                <button type="button" onClick={copy} aria-label="复制" title="复制" className="ml-auto flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary transition hover:bg-primary hover:text-fg-quaternary_hover">
                    {copied ? <Check className="size-3.5 text-fg-success-primary" /> : <Copy01 className="size-3.5" />}
                </button>
            </div>
            <pre style={{ maxHeight }} className={`overflow-auto whitespace-pre-wrap break-words font-mono ${density === "reading" ? "px-4 py-3 text-sm leading-7" : "px-3 py-2 text-[12px] leading-relaxed"} [overflow-wrap:anywhere] ${muted ? "text-tertiary" : "text-secondary"}`}><code data-md-start={sourceStart} data-md-end={sourceEnd} data-md-kind="code">{code}</code></pre>
        </div>
    );
}
