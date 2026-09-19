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
    /** Which side of the pane the handle sits on. A pane docked to the
     *  right is resized from its left edge, where moving the pointer
     *  left makes it wider; the default handle is on the right edge. */
    edge?: "start" | "end";
}

export function PaneResizer({ width, onChange, min, max, initial, label, edge = "end" }: PaneResizerProps) {
    const [dragging, setDragging] = useState(false);
    const origin = useRef({ x: 0, width });
    const grow = edge === "start" ? -1 : 1;

    // The drag is followed on the window, not the handle: a pointer that
    // outruns the edge, or leaves it entirely, still moves the pane.
    useEffect(() => {
        if (!dragging) return;
        const move = (event: PointerEvent) => onChange(origin.current.width + grow * (event.clientX - origin.current.x));
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
    }, [dragging, onChange, grow]);

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
            className={`pane-resizer${edge === "start" ? " is-start" : ""}${dragging ? " is-dragging" : ""}`}
            onPointerDown={(event) => {
                if (event.button !== 0) return;
                event.preventDefault();
                origin.current = { x: event.clientX, width };
                setDragging(true);
            }}
            onDoubleClick={() => onChange(initial)}
            onKeyDown={(event) => {
                const step = event.shiftKey ? 32 : 8;
                if (event.key === "ArrowLeft") { event.preventDefault(); onChange(width - grow * step); }
                else if (event.key === "ArrowRight") { event.preventDefault(); onChange(width + grow * step); }
                else if (event.key === "Home") { event.preventDefault(); onChange(initial); }
            }}
        />
    );
}
