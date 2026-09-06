import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { Check, Copy01 } from "@untitledui/icons";
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
    lang?: string;
    readingState?: SourceReadingState;
    onReadingStateChange?: (state: SourceReadingState) => void;
}

export function SourceView(props: SourceViewProps) {
    const { file } = props;
    return <SourceContent key={`${file.attempt}:${file.commit}:${file.path}`} {...props} />;
}

function SourceContent({ file, lang, readingState, onReadingStateChange }: SourceViewProps) {
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
        <section className="source-view" aria-label={`源码 ${file.path}`}>
            <div className="source-toolbar">
                <div className="source-file-meta"><span>{file.binary ? "二进制" : langName(lang || (language === "ini" && file.path.endsWith(".toml") ? "toml" : language === "xml" && /\.html?$/i.test(file.path) ? "html" : language))}</span><span>{size}</span>{!file.binary && <span>{total} 行{file.truncated ? "（已取回）" : ""}</span>}<span>只读</span></div>
                {!file.binary && <div className="source-actions">
                    <Button size="xs" color="tertiary" aria-pressed={wrap} onClick={() => { remember({ wrap: !wrap }); setWrap(!wrap); }}>自动换行</Button>
                    <Button size="xs" color="tertiary" iconLeading={copyState === "copied" ? Check : Copy01} isLoading={copyState === "copying"} isDisabled={!file.text} onClick={() => void copy()}>{copyState === "copied" ? "已复制" : "复制"}</Button>
                </div>}
            </div>
            <div className={`source-copy-status ${copyState === "failed" ? "" : "sr-only"}`} role="status" aria-live="polite">{copyState === "copied" ? (file.truncated ? "已复制取回的部分内容" : "已复制文件内容") : copyState === "failed" ? "复制失败，请允许剪贴板权限，或选中文本手动复制。" : ""}</div>
            {file.truncated && <p className="source-notice" role="status">文件较大，仅取回部分内容。当前浏览与复制均不包含未取回的部分。</p>}
            {file.binary ? <div className="source-empty"><p>二进制文件</p><span>无法显示为源码。</span></div> : !file.text ? <div className="source-empty"><p>空文件</p><span>此文件没有文本内容。</span></div> : (
                <div ref={scroll} onScroll={() => remember()} className={`source-scroll ${wrap ? "source-wrap" : ""}`} tabIndex={0} role="region" aria-label={`${file.path} 文件内容`}>
                    <pre className="source-code"><code>{visible.map((html, index) => <span className="source-line" key={index}><span className="source-number" aria-hidden="true">{index + 1}</span><span className="source-text" dangerouslySetInnerHTML={{ __html: html || "\n" }} /></span>)}</code></pre>
                    {remaining > 0 && <div className="source-more"><span>已显示 {Math.min(limit, total)} / {total} 行</span><Button size="sm" color="secondary" onClick={() => { remember({ limit: limit + pageSize }); setLimit(limit + pageSize); }}>继续加载 {Math.min(pageSize, remaining)} 行</Button></div>}
                </div>
            )}
        </section>
    );
}
