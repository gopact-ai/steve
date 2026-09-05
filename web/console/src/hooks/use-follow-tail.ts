import { useLayoutEffect, useRef, type KeyboardEvent, type UIEvent } from "react";

// Follow streamed text until the reader scrolls it. Shared by legacy
// thinking folds and timeline summaries so rerenders never reset intent.
export function useFollowTail(text: string, live?: boolean) {
    const ref = useRef<HTMLDivElement>(null);
    const manuallyScrolled = useRef(false);
    const autoTop = useRef(0);
    const followTail = () => {
        const el = ref.current;
        if (live && el && !manuallyScrolled.current && el.clientHeight) {
            el.scrollTop = el.scrollHeight;
            autoTop.current = el.scrollTop;
        }
    };
    useLayoutEffect(() => {
        followTail();
        // A running child can become visible when an outer process fold
        // opens, without receiving another text chunk at that moment.
        const observer = new ResizeObserver(followTail);
        if (ref.current) observer.observe(ref.current);
        return () => observer.disconnect();
    }, [text, live]);
    return {
        ref, followTail,
        onWheel: () => { manuallyScrolled.current = true; },
        onTouchMove: () => { manuallyScrolled.current = true; },
        onKeyDown: (e: KeyboardEvent<HTMLDivElement>) => { if (["ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End", " "].includes(e.key)) manuallyScrolled.current = true; },
        onScroll: (e: UIEvent<HTMLDivElement>) => { if (Math.abs(e.currentTarget.scrollTop - autoTop.current) > 1) manuallyScrolled.current = true; },
    };
}
