import { useState, type DetailsHTMLAttributes, type ReactNode } from "react";

// Native details owns visibility; React only remembers whether its body has
// ever been needed. Retaining that body preserves state, DOM and source ranges.
export function DeferredDetails({ summary, children, open, onToggle, ...props }: DetailsHTMLAttributes<HTMLDetailsElement> & { summary: ReactNode }) {
    const [mounted, setMounted] = useState(!!open);
    // Remember prop-driven opens during render, even if closed again before
    // the browser dispatches its (coalesced, asynchronous) toggle event.
    if (open && !mounted) setMounted(true);
    return (
        <details {...props} open={open} onToggle={(event) => {
            if (event.target !== event.currentTarget) return;
            if (event.currentTarget.open) setMounted(true);
            onToggle?.(event);
        }}>
            {summary}
            {mounted ? children : null}
        </details>
    );
}
