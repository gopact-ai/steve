import { useEffect, useLayoutEffect, useRef } from "react";

// A short notice at the bottom of the window for the outcome of an action
// whose own control has nowhere to show it. It dismisses itself.
export function FloatingNotice({ text, closeLabel, onClose }: { text: string; closeLabel: string; onClose: () => void }) {
    const close = useRef(onClose);
    useLayoutEffect(() => { close.current = onClose; });
    useEffect(() => {
        const timer = window.setTimeout(() => close.current(), 4000);
        return () => window.clearTimeout(timer);
    }, [text]);
    return <div role="status" className="pointer-events-none fixed bottom-4 left-1/2 -translate-x-1/2 z-[140] flex max-w-sm items-center gap-3 rounded-lg border border-secondary bg-primary p-3 text-sm text-primary shadow-lg"><span>{text}</span><button type="button" className="pointer-events-auto" aria-label={closeLabel} onClick={onClose}>×</button></div>;
}
