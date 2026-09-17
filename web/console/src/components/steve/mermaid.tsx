import { useEffect, useRef, useState } from "react";
import { useI18n } from "@/providers/locale-provider";
import { CodeBlock } from "./code-block";

// Mermaid draws a fenced mermaid block as the diagram it describes. The
// library is a megabyte of layout engines, so it is imported the first
// time a diagram appears and never from the initial bundle. A diagram
// that does not parse — the usual case while an answer is still
// streaming — keeps showing its text instead of an error.
let engine: Promise<typeof import("mermaid")["default"]> | null = null;
let themed = "";

function load(dark: boolean) {
    const want = dark ? "dark" : "light";
    if (!engine || themed !== want) {
        themed = want;
        engine = import("mermaid").then(({ default: mermaid }) => {
            mermaid.initialize({
                startOnLoad: false,
                securityLevel: "strict",
                theme: dark ? "dark" : "default",
                fontFamily: "inherit",
                suppressErrorRendering: true,
            });
            return mermaid;
        });
    }
    return engine;
}

function isDark() {
    return typeof document !== "undefined" && document.documentElement.classList.contains("dark-mode");
}

// useDark follows the theme class rather than the stored preference, so
// "system" and a change of system appearance both reach the diagram.
function useDark() {
    const [dark, setDark] = useState(isDark);
    useEffect(() => {
        const observer = new MutationObserver(() => setDark(isDark()));
        observer.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
        return () => observer.disconnect();
    }, []);
    return dark;
}

let counter = 0;

export function Mermaid({ code, className }: { code: string; className?: string }) {
    const { t } = useI18n();
    const dark = useDark();
    const [svg, setSvg] = useState("");
    const [failed, setFailed] = useState(false);
    const id = useRef(`mermaid-${++counter}`);
    useEffect(() => {
        let gone = false;
        const text = code.trim();
        if (!text) { setSvg(""); setFailed(false); return; }
        void (async () => {
            try {
                const mermaid = await load(dark);
                if (gone) return;
                const rendered = await mermaid.render(`${id.current}-${dark ? "d" : "l"}`, text);
                if (gone) return;
                setSvg(rendered.svg);
                setFailed(false);
            } catch {
                if (!gone) { setSvg(""); setFailed(true); }
            }
        })();
        return () => { gone = true; };
    }, [code, dark]);
    if (svg) {
        return (
            <figure className={`mermaid-figure not-prose ${className ?? ""}`} aria-label={t("console.diagram")}>
                <div className="mermaid-canvas" dangerouslySetInnerHTML={{ __html: svg }} />
                <details className="mermaid-source">
                    <summary>{t("console.diagramSource")}</summary>
                    <CodeBlock code={code} lang="mermaid" />
                </details>
            </figure>
        );
    }
    return <CodeBlock code={code} lang="mermaid" label={failed ? t("console.diagramFailed") : t("console.diagramDrawing")} />;
}
