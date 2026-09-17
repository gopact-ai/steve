import { useCallback, useState } from "react";

// A pane's width is a preference, not a constant: it belongs to the
// person reading, and it should survive a reload. A width remembered
// from a wider window is clamped on the way back in, so a narrow screen
// never opens with a pane it cannot show.
export function usePaneWidth(key: string, initial: number, min: number, max: number) {
    const clamp = useCallback((value: number) => Math.min(max, Math.max(min, Math.round(value))), [min, max]);
    const [width, setWidth] = useState(() => {
        try {
            const saved = Number(localStorage.getItem(key));
            return Number.isFinite(saved) && saved > 0 ? Math.min(max, Math.max(min, Math.round(saved))) : initial;
        } catch { return initial; }
    });
    const change = useCallback((next: number) => {
        const value = Math.min(max, Math.max(min, Math.round(next)));
        setWidth(value);
        try { localStorage.setItem(key, String(value)); } catch { /* The preference is optional. */ }
    }, [key, min, max]);
    return [width, change, clamp] as const;
}
