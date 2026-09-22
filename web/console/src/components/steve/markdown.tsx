import { createContext, memo, useContext, useMemo, type AnchorHTMLAttributes, type ReactElement, type ReactNode } from "react";
import Markdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Element, Root, Text } from "hast";
import type { Plugin } from "unified";
import { useTranslator } from "@/providers/locale-provider";
import { CodeBlock } from "./code-block";
import { Mermaid } from "./mermaid";

// A document links to its neighbours with paths that mean something to the
// file it came from, not to this page's address. Following one in the
// browser walks out of the console and lands on a blank page, so a reader
// that knows where the text lives says so here and gets the links opened
// in place instead.
export interface DocumentLinks {
    // base is the path of the document being read, links resolve against it.
    base: string;
    open: (path: string) => void;
}

const documentLinks = createContext<DocumentLinks | null>(null);

export function MarkdownDocument({ base, open, children }: DocumentLinks & { children: ReactNode }) {
    const value = useMemo(() => ({ base, open }), [base, open]);
    return <documentLinks.Provider value={value}>{children}</documentLinks.Provider>;
}

// Md renders a piece of markdown the way the transcript wants it: the
// prose rules from globals.css, and every fenced block as a CodeBlock
// with its language named and a copy button, like a chat app's.
export const Md = memo(function Md({ text, size = "sm", variant = "default", className }: { text: string; size?: "sm" | "xs"; variant?: "default" | "conversation"; className?: string }) {
    return (
        <div className={`md prose prose-sm max-w-none break-words [overflow-wrap:anywhere] ${variant === "conversation" ? "md-conversation" : ""} ${size === "xs" ? "text-xs prose-p:my-0.5 prose-strong:font-medium" : ""} ${className ?? ""}`}>
            <Markdown remarkPlugins={[remarkGfm]} rehypePlugins={[sourcePositions]} components={variant === "conversation" ? conversationComponents : components}>{text}</Markdown>
        </div>
    );
});

const markdownComponents = (conversation: boolean): Components => ({
    a: MdLink,
    pre: ({ children }) => {
        const el = children as ReactElement<{ className?: string; children?: ReactNode; "data-md-start"?: number; "data-md-end"?: number }> | undefined;
        if (!el || typeof el !== "object" || !("props" in el)) return <pre>{children}</pre>;
        const lang = /language-([\w+#.-]+)/.exec(el.props.className || "")?.[1];
        const code = String(el.props.children ?? "").replace(/\n$/, "");
        // A diagram is worth drawing wherever it appears: an answer, a
        // report, a file. The block keeps its text one fold away.
        if (lang === "mermaid") return <Mermaid code={code} />;
        return <CodeBlock lang={lang} code={code} density={conversation ? "reading" : "compact"} sourceStart={el.props["data-md-start"]} sourceEnd={el.props["data-md-end"]} />;
    },
});
const components = markdownComponents(false);
const conversationComponents = markdownComponents(true);

// A link is followed where it leads somewhere this app can go: the web
// opens in a new tab so the console stays put, a neighbouring file opens in
// the reader when one is listening, and anything else stays readable text
// with its target in the tooltip rather than a navigation to nowhere.
function MdLink({ href, children, node: _node, ...rest }: AnchorHTMLAttributes<HTMLAnchorElement> & { node?: unknown }) {
    const t = useTranslator();
    const reader = useContext(documentLinks);
    const target = (href ?? "").trim();
    if (/^(https?:|mailto:|tel:)/i.test(target)) return <a {...rest} href={target} target="_blank" rel="noreferrer noopener">{children}</a>;
    const path = reader && !/^[a-z][a-z0-9+.-]*:/i.test(target) ? resolveDocumentPath(reader.base, target) : null;
    if (!path) return <span className="md-link-inert" title={target ? t("console.linkNotOpenable", { href: target }) : undefined}>{children}</span>;
    return <button type="button" className="md-link" title={t("console.openLinkedFile", { path })} onClick={() => reader!.open(path)}>{children}</button>;
}

// Resolve a link the way a file browser would: relative to the folder of
// the document, absolute against the root of the snapshot, and refusing to
// climb out of it. A pure anchor or query has no file to open.
export function resolveDocumentPath(base: string, href: string): string {
    const [address] = href.split(/[?#]/, 1);
    if (!address) return "";
    let raw = address;
    try { raw = decodeURI(address); } catch { /* A malformed escape is taken literally. */ }
    const rooted = raw.startsWith("/");
    const from = rooted || !base.includes("/") ? [] : base.slice(0, base.lastIndexOf("/")).split("/");
    const segments = rooted ? [] : from.filter((part) => part && part !== ".");
    for (const part of raw.split("/")) {
        if (!part || part === ".") continue;
        if (part !== "..") { segments.push(part); continue; }
        if (!segments.length) return "";
        segments.pop();
    }
    return segments.join("/");
}

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
