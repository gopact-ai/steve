import { useState, type ReactElement, type ReactNode } from "react";
import Markdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { Check, Code02, Copy01, Terminal } from "@untitledui/icons";
import type { Element, Root, Text } from "hast";
import type { Plugin } from "unified";

// Md renders a piece of markdown the way the transcript wants it: the
// prose rules from globals.css, and every fenced block as a CodeBlock
// with its language named and a copy button, like a chat app's.
export function Md({ text, size = "sm", className }: { text: string; size?: "sm" | "xs"; className?: string }) {
    return (
        <div className={`md prose prose-sm max-w-none break-words [overflow-wrap:anywhere] ${size === "xs" ? "text-xs prose-p:my-0.5 prose-strong:font-medium" : ""} ${className ?? ""}`}>
            <Markdown remarkPlugins={[remarkGfm]} rehypePlugins={[sourcePositions]} components={components}>{text}</Markdown>
        </div>
    );
}

const components: Components = {
    pre: ({ children }) => {
        const el = children as ReactElement<{ className?: string; children?: ReactNode; "data-md-start"?: number; "data-md-end"?: number }> | undefined;
        if (!el || typeof el !== "object" || !("props" in el)) return <pre>{children}</pre>;
        const lang = /language-([\w+#.-]+)/.exec(el.props.className || "")?.[1];
        return <CodeBlock lang={lang} code={String(el.props.children ?? "").replace(/\n$/, "")} sourceStart={el.props["data-md-start"]} sourceEnd={el.props["data-md-end"]} />;
    },
};

// Preserve parser source offsets before presenting hard line breaks. Running
// remark-breaks first discards positions on the text fragments it creates.
const sourcePositions: Plugin<[], Root> = () => (tree, file) => {
    const raw = String(file.value);
    const bounded = raw.length <= 2 << 20;
    const stack: (Root | Element)[] = [tree];
    let visited = 0;
    while (stack.length) {
        const parent = stack.pop()!;
        for (let i = 0; i < parent.children.length; i++) {
            const annotate = bounded && ++visited <= 20_000;
            const child = parent.children[i];
            if (child.type === "element") {
                if (parent.type === "element" && parent.tagName === "pre" && child.tagName === "code") {
                    const start = child.position?.start.offset, end = child.position?.end.offset;
                    const code = child.children.map((node) => node.type === "text" ? node.value : "").join("").replace(/\n$/, "");
                    if (annotate && start !== undefined && end !== undefined) {
                        const span = codeSpan(raw, start, end, code);
                        if (span) Object.assign(child.properties, { "data-md-start": span[0], "data-md-end": span[1] });
                    }
                } else stack.push(child);
            } else if (child.type === "text") {
                const start = child.position?.start.offset, end = child.position?.end.offset;
                if (start === undefined || end === undefined) continue;
                const inline = parent.type === "element" && parent.tagName === "code";
                const children: (Element | Text)[] = [];
                if (inline) children.push(child);
                else child.value.split(/\r\n|\r|\n/).forEach((value, index) => {
                    if (index) children.push({ type: "element", tagName: "br", properties: {}, children: [] }, { type: "text", value: "\n" });
                    if (value) children.push({ type: "text", value });
                });
                if (annotate) parent.children[i] = { type: "element", tagName: "span", properties: { "data-md-start": start, "data-md-end": end, "data-md-kind": inline ? "inline-code" : "text" }, children };
                else { parent.children.splice(i, 1, ...children); i += children.length - 1; }
            }
        }
    }
};

function codeSpan(raw: string, start: number, end: number, code: string): [number, number] | null {
    const source = raw.slice(start, end);
    const opening = /^ {0,3}(`{3,}|~{3,})[^\r\n]*(?:\r\n|\r|\n)/.exec(source);
    if (!opening) return source.replace(/\r\n|\r/g, "\n") === code.replace(/\r\n|\r/g, "\n") ? [start, end] : null;
    start += opening[0].length;
    const lastLine = Math.max(raw.lastIndexOf("\n", end - 1), raw.lastIndexOf("\r", end - 1)) + 1;
    const closing = /^ {0,3}(`{3,}|~{3,})[ \t]*$/.exec(raw.slice(lastLine, end));
    if (closing && closing[1][0] === opening[1][0] && closing[1].length >= opening[1].length) end = lastLine;
    if (raw.slice(end - 2, end) === "\r\n") end -= 2;
    else if (/[\r\n]/.test(raw[end - 1] || "")) end--;
    return end >= start && raw.slice(start, end).replace(/\r\n|\r/g, "\n") === code.replace(/\r\n|\r/g, "\n") ? [start, end] : null;
}

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
export function CodeBlock({ code, lang, label, meta, muted, maxHeight = 320, className, sourceStart, sourceEnd }: { code: string; lang?: string; label?: ReactNode; meta?: ReactNode; muted?: boolean; maxHeight?: number; className?: string; sourceStart?: number; sourceEnd?: number }) {
    const [copied, setCopied] = useState(false);
    const shell = lang === "shell" || lang === "sh" || lang === "bash" || lang === "console";
    const Icon = shell ? Terminal : Code02;
    function copy() {
        void navigator.clipboard?.writeText(code).then(() => { setCopied(true); window.setTimeout(() => setCopied(false), 1500); }).catch(() => undefined);
    }
    return (
        <div className={`not-prose my-2 flex min-w-0 flex-col overflow-hidden rounded-lg bg-secondary ring-1 ring-secondary ${className ?? ""}`}>
            <div data-selection-ignore className="flex items-center gap-1.5 border-b border-secondary px-3 py-1 u-meta text-quaternary">
                <Icon className="size-3.5 shrink-0" />
                <span className="font-medium">{label ?? langName(lang)}</span>
                {meta && <span className="truncate">· {meta}</span>}
                <button type="button" onClick={copy} aria-label="复制" title="复制" className="ml-auto flex size-5 shrink-0 items-center justify-center rounded text-fg-quaternary transition hover:bg-primary hover:text-fg-quaternary_hover">
                    {copied ? <Check className="size-3.5 text-fg-success-primary" /> : <Copy01 className="size-3.5" />}
                </button>
            </div>
            <pre style={{ maxHeight }} className={`overflow-auto whitespace-pre-wrap break-words px-3 py-2 font-mono text-[12px] leading-relaxed [overflow-wrap:anywhere] ${muted ? "text-tertiary" : "text-secondary"}`}><code data-md-start={sourceStart} data-md-end={sourceEnd} data-md-kind="code">{code}</code></pre>
        </div>
    );
}
