import { useEffect, useRef, useState } from "react";

// The handle sits on the pane's own border: invisible until the pointer
// comes near, then a line under a target wide enough to hit. Dragging
// moves the edge, the arrow keys move it for anyone without a mouse,
// and a double click puts it back where it started.

interface PaneResizerProps {
    width: number;
    onChange: (width: number) => void;
    min: number;
    max: number;
    /** The width a double click, or Home, returns to. */
    initial: number;
    label: string;
}

export function PaneResizer({ width, onChange, min, max, initial, label }: PaneResizerProps) {
    const [dragging, setDragging] = useState(false);
    const origin = useRef({ x: 0, width });

    // The drag is followed on the window, not the handle: a pointer that
    // outruns the edge, or leaves it entirely, still moves the pane.
    useEffect(() => {
        if (!dragging) return;
        const move = (event: PointerEvent) => onChange(origin.current.width + event.clientX - origin.current.x);
        const stop = () => setDragging(false);
        window.addEventListener("pointermove", move);
        window.addEventListener("pointerup", stop);
        window.addEventListener("pointercancel", stop);
        document.body.classList.add("is-resizing-pane");
        return () => {
            window.removeEventListener("pointermove", move);
            window.removeEventListener("pointerup", stop);
            window.removeEventListener("pointercancel", stop);
            document.body.classList.remove("is-resizing-pane");
        };
    }, [dragging, onChange]);

    return (
        <div
            role="separator"
            aria-orientation="vertical"
            aria-label={label}
            aria-valuenow={width}
            aria-valuemin={min}
            aria-valuemax={max}
            tabIndex={0}
            title={label}
            className={`pane-resizer${dragging ? " is-dragging" : ""}`}
            onPointerDown={(event) => {
                if (event.button !== 0) return;
                event.preventDefault();
                origin.current = { x: event.clientX, width };
                setDragging(true);
            }}
            onDoubleClick={() => onChange(initial)}
            onKeyDown={(event) => {
                const step = event.shiftKey ? 32 : 8;
                if (event.key === "ArrowLeft") { event.preventDefault(); onChange(width - step); }
                else if (event.key === "ArrowRight") { event.preventDefault(); onChange(width + step); }
                else if (event.key === "Home") { event.preventDefault(); onChange(initial); }
            }}
        />
    );
}
