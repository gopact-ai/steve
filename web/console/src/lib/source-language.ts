import hljs from "highlight.js/lib/core";
import bash from "highlight.js/lib/languages/bash";
import css from "highlight.js/lib/languages/css";
import diff from "highlight.js/lib/languages/diff";
import go from "highlight.js/lib/languages/go";
import ini from "highlight.js/lib/languages/ini";
import javascript from "highlight.js/lib/languages/javascript";
import json from "highlight.js/lib/languages/json";
import markdown from "highlight.js/lib/languages/markdown";
import python from "highlight.js/lib/languages/python";
import rust from "highlight.js/lib/languages/rust";
import sql from "highlight.js/lib/languages/sql";
import typescript from "highlight.js/lib/languages/typescript";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";

for (const [name, grammar] of Object.entries({ bash, css, diff, go, ini, javascript, json, markdown, python, rust, sql, typescript, xml, yaml })) {
    hljs.registerLanguage(name, grammar);
}

const languages: Record<string, string> = {
    sh: "bash", bash: "bash", zsh: "bash", shell: "bash", bashrc: "bash", zshrc: "bash",
    go: "go", js: "javascript", jsx: "javascript", mjs: "javascript", cjs: "javascript", javascript: "javascript",
    ts: "typescript", tsx: "typescript", mts: "typescript", cts: "typescript", typescript: "typescript",
    py: "python", python: "python", rs: "rust", rust: "rust", sql: "sql",
    json: "json", yaml: "yaml", yml: "yaml", toml: "ini", ini: "ini", cfg: "ini",
    md: "markdown", markdown: "markdown", css: "css", html: "xml", htm: "xml", xml: "xml", svg: "xml",
    diff: "diff", patch: "diff",
};

export function sourceLanguage(path: string, lang?: string): string | undefined {
    const name = path.split("/").pop()?.toLowerCase() || "";
    return languages[(lang || name.split(".").pop() || "").toLowerCase()];
}

const escape = (text: string) => text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#x27;");

// Highlight a complete prefix so multiline comments and strings retain their
// context, then close and reopen generated spans at each displayed line.
export function sourceLines(text: string, language?: string): string[] {
    let html = escape(text);
    if (language && hljs.getLanguage(language)) {
        try { html = hljs.highlight(text, { language, ignoreIllegals: true }).value; } catch { /* Plain text remains readable if a grammar rejects the input. */ }
    }
    const lines: string[] = [];
    const spans: string[] = [];
    let line = "";
    for (const part of html.split(/(\n|<span class="[^"]*">|<\/span>)/)) {
        if (part === "\n") {
            lines.push(line + "</span>".repeat(spans.length));
            line = spans.join("");
        } else {
            if (part.startsWith("<span ")) spans.push(part);
            else if (part === "</span>") spans.pop();
            line += part;
        }
    }
    lines.push(line);
    return lines;
}
