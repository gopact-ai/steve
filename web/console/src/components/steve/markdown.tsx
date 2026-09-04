import { useState, type ReactElement, type ReactNode } from "react";
import Markdown, { type Components } from "react-markdown";
import remarkBreaks from "remark-breaks";
import { Check, Code02, Copy01, Terminal } from "@untitledui/icons";

// Md renders a piece of markdown the way the transcript wants it: the
// prose rules from globals.css, and every fenced block as a CodeBlock
// with its language named and a copy button, like a chat app's.
export function Md({ text, size = "sm", className }: { text: string; size?: "sm" | "xs"; className?: string }) {
    return (
        <div className={`md prose prose-sm max-w-none break-words [overflow-wrap:anywhere] ${size === "xs" ? "text-xs prose-p:my-0.5 prose-strong:font-medium" : ""} ${className ?? ""}`}>
            <Markdown remarkPlugins={[remarkBreaks]} components={components}>{text}</Markdown>
        </div>
    );
}

const components: Components = {
    pre: ({ children }) => {
        const el = children as ReactElement<{ className?: string; children?: ReactNode }> | undefined;
        if (!el || typeof el !== "object" || !("props" in el)) return <pre>{children}</pre>;
        const lang = /language-([\w+#.-]+)/.exec(el.props.className || "")?.[1];
        return <CodeBlock lang={lang} code={String(el.props.children ?? "").replace(/\n$/, "")} />;
    },
};

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
export function CodeBlock({ code, lang, label, meta, muted, maxHeight = 320, className }: { code: string; lang?: string; label?: ReactNode; meta?: ReactNode; muted?: boolean; maxHeight?: number; className?: string }) {
    const [copied, setCopied] = useState(false);
    const shell = lang === "shell" || lang === "sh" || lang === "bash" || lang === "console";
    const Icon = shell ? Terminal : Code02;
    function copy() {
        void navigator.clipboard?.writeText(code).then(() => { setCopied(true); window.setTimeout(() => setCopied(false), 1500); }).catch(() => undefined);
    }
    return (
        <div className={`not-prose my-2 flex min-w-0 flex-col overflow-hidden rounded-lg bg-secondary ring-1 ring-secondary ${className ?? ""}`}>
            <div className="flex items-center gap-1.5 border-b border-secondary px-3 py-1 text-[11px] text-quaternary">
                <Icon className="size-3.5 shrink-0" />
                <span className="font-medium">{label ?? langName(lang)}</span>
                {meta && <span className="truncate">· {meta}</span>}
                <button type="button" onClick={copy} aria-label="复制" title="复制" className="ml-auto flex size-5 shrink-0 items-center justify-center rounded text-fg-quaternary transition hover:bg-primary hover:text-fg-quaternary_hover">
                    {copied ? <Check className="size-3.5 text-fg-success-primary" /> : <Copy01 className="size-3.5" />}
                </button>
            </div>
            <pre style={{ maxHeight }} className={`overflow-auto whitespace-pre-wrap break-words px-3 py-2 font-mono text-[12px] leading-relaxed [overflow-wrap:anywhere] ${muted ? "text-tertiary" : "text-secondary"}`}><code>{code}</code></pre>
        </div>
    );
}
