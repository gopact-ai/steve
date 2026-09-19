import { createContext, useCallback, useContext, useEffect, useRef, type ReactNode } from "react";

// Closing peels the workbench one layer at a time. A reader who opened a
// file inside a snapshot, beside a side chat, beside a details rail, means
// "close this" long before they mean "close the window", so the shortcut
// takes the frontmost thing first and only reaches the window once there
// is nothing left on screen to put away.
//
// Layers announce themselves while they are visible and are ranked the way
// they are seen: whatever sits on top goes first, and among panels standing
// side by side the right-hand one goes before what it sits beside.
export const closeOrder = {
    /** A dialog covers everything, including the page that opened it. */
    dialog: 800,
    /** Inside the snapshot workspace the chat stands to the right of the
     *  file being read, so it goes first, then the file, then the
     *  workspace that held both. */
    reviewSide: 720,
    reviewFile: 710,
    review: 700,
    /** Drawers and sheets that slide over a page. */
    sheet: 600,
    /** The console's own panels, right to left. */
    inspector: 300,
    splitTab: 200,
} as const;

interface Layer { rank: number; seq: number; run: () => void }

interface Stack {
    add: (layer: Layer) => () => void;
    next: () => number;
    /** Closes the frontmost layer and reports whether one was there. */
    pop: () => boolean;
}

const Context = createContext<Stack | null>(null);

declare global {
    interface Window {
        /** The macOS shell asks the page first, because the window menu
         *  owns Cmd+W before the page can ever see the key. */
        steveCloseLayer?: () => boolean;
    }
}

export function CloseStackProvider({ children }: { children: ReactNode }) {
    const layers = useRef<Set<Layer>>(new Set());
    const counter = useRef(0);
    const add = useCallback((layer: Layer) => {
        layers.current.add(layer);
        return () => { layers.current.delete(layer); };
    }, []);
    const next = useCallback(() => ++counter.current, []);
    const pop = useCallback(() => {
        let front: Layer | null = null;
        // Same rank means siblings, and the one opened last is the one in
        // front, so registration order settles the tie.
        for (const layer of layers.current) if (!front || layer.rank > front.rank || (layer.rank === front.rank && layer.seq > front.seq)) front = layer;
        if (!front) return false;
        front.run();
        return true;
    }, []);
    useEffect(() => {
        const key = (event: KeyboardEvent) => {
            if (event.key !== "w" && event.key !== "W") return;
            if (!(event.metaKey || event.ctrlKey) || event.altKey || event.shiftKey) return;
            if (!pop()) return;
            event.preventDefault();
            event.stopPropagation();
        };
        document.addEventListener("keydown", key, true);
        window.steveCloseLayer = pop;
        return () => {
            document.removeEventListener("keydown", key, true);
            if (window.steveCloseLayer === pop) delete window.steveCloseLayer;
        };
    }, [pop]);
    const value = useRef<Stack>({ add, next, pop });
    value.current = { add, next, pop };
    return <Context.Provider value={value.current}>{children}</Context.Provider>;
}

/** useCloseLayer keeps a panel in the closing order while it is on screen.
 *  A layer that cannot be dismissed yet still registers, with a close that
 *  does nothing, so the shortcut stops at it rather than reaching past it
 *  and closing whatever it happens to be covering. */
export function useCloseLayer(rank: number, close: () => void, active = true) {
    const stack = useContext(Context);
    const latest = useRef(close);
    latest.current = close;
    useEffect(() => {
        if (!stack || !active) return;
        return stack.add({ rank, seq: stack.next(), run: () => latest.current() });
    }, [stack, rank, active]);
}
