import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";

const storageKey = "steve.inspector.width";
const defaultWidth = 336;
const minWidth = 320;
const widthLimit = 640;
const conversationMinimum = 480;

function readWidth() {
    try {
        const stored = Number(localStorage.getItem(storageKey));
        if (Number.isFinite(stored) && stored >= minWidth) return Math.min(stored, widthLimit);
    } catch { /* Use the default when browser storage is unavailable. */ }
    return defaultWidth;
}

export function ResizableInspector({ children, overlay = false }: { children: ReactNode; overlay?: boolean }) {
    const id = useId();
    const panel = useRef<HTMLDivElement>(null);
    const separator = useRef<HTMLDivElement>(null);
    const drag = useRef<{ x: number; width: number; preferred: number; pointer: number } | null>(null);
    const [preferred, setPreferred] = useState(readWidth);
    const [maximum, setMaximum] = useState(widthLimit);
    const [dragging, setDragging] = useState(false);
    const width = Math.min(preferred, maximum);
    const clamp = (value: number) => Math.round(Math.max(minWidth, Math.min(maximum, value)));

    useLayoutEffect(() => {
        const container = overlay ? document.documentElement : panel.current?.parentElement;
        if (!container) return;
        const measure = () => setMaximum(Math.max(minWidth, Math.min(widthLimit, container.clientWidth - (overlay ? 64 : conversationMinimum))));
        measure();
        const observer = new ResizeObserver(measure);
        observer.observe(container);
        return () => observer.disconnect();
    }, [overlay]);

    useEffect(() => {
        if (!dragging) return;
        const { cursor, userSelect } = document.body.style;
        document.body.style.cursor = "col-resize";
        document.body.style.userSelect = "none";
        return () => { document.body.style.cursor = cursor; document.body.style.userSelect = userSelect; };
    }, [dragging]);

    function commit(value: number) {
        const next = clamp(value);
        setPreferred(next);
        try { localStorage.setItem(storageKey, String(next)); } catch { /* Retain the width for this open panel. */ }
    }
    function finish(cancel = false, x?: number) {
        const current = drag.current;
        if (!current) return;
        drag.current = null;
        if (cancel) setPreferred(current.preferred);
        else commit(x === undefined ? width : current.width + current.x - x);
        setDragging(false);
        if (separator.current?.hasPointerCapture(current.pointer)) separator.current.releasePointerCapture(current.pointer);
    }

    return (
        <div ref={panel} id={id} className="resizable-inspector" style={{ width }}>
            <div ref={separator} role="separator" aria-label="调整详情栏宽度" aria-orientation="vertical" aria-controls={id}
                aria-valuemin={minWidth} aria-valuemax={maximum} aria-valuenow={width} aria-valuetext={`${width} 像素`} tabIndex={0}
                title="拖动调整宽度；方向键微调；双击还原" className={`inspector-resize-handle ${dragging ? "is-dragging" : ""}`}
                onPointerDown={(event) => {
                    if (event.button !== 0 || drag.current) return;
                    event.preventDefault();
                    event.currentTarget.focus({ preventScroll: true });
                    event.currentTarget.setPointerCapture(event.pointerId);
                    drag.current = { x: event.clientX, width, preferred, pointer: event.pointerId };
                    setDragging(true);
                }}
                onPointerMove={(event) => {
                    if (drag.current?.pointer === event.pointerId) setPreferred(clamp(drag.current.width + drag.current.x - event.clientX));
                }}
                onPointerUp={(event) => { if (drag.current?.pointer === event.pointerId) finish(false, event.clientX); }}
                onPointerCancel={(event) => { if (drag.current?.pointer === event.pointerId) finish(true); }} onLostPointerCapture={() => finish()}
                onDoubleClick={() => commit(defaultWidth)}
                onKeyDown={(event) => {
                    if (event.key === "Escape" && drag.current) { event.preventDefault(); event.stopPropagation(); finish(true); return; }
                    if (drag.current) return;
                    const next = event.key === "ArrowLeft" ? width + 16 : event.key === "ArrowRight" ? width - 16 : event.key === "Home" ? minWidth : event.key === "End" ? maximum : event.key === "Enter" ? defaultWidth : null;
                    if (next !== null) { event.preventDefault(); commit(next); }
                }} />
            {children}
        </div>
    );
}
