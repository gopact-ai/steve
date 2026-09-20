import { Component, Suspense, useEffect, useLayoutEffect, useMemo, useRef, type ReactNode, type RefObject } from "react";
import { Button } from "@/components/base/buttons/button";
import { closeOrder, useCloseLayer } from "@/providers/close-stack";
import { useI18n } from "@/providers/locale-provider";

interface LazyRegionProps {
    children: ReactNode;
    label?: string;
    resetKey?: string;
    onClose?: () => void;
}

// A failed optional view must not take its surrounding workbench with it.
// Navigation clears the error without remounting healthy children (and drafts).
// Import failures are cached by React/browser: closing is not a fake retry.
export function LazyRegion({ children, label, resetKey, onClose }: LazyRegionProps) {
    const { t } = useI18n();
    // Loading and error notices share the trigger even when one replaces the
    // other. Keep it through a loaded dialog whose original notice disappears.
    const returnFocus = useMemo<RefObject<HTMLElement | null>>(() => ({ current: null }), [resetKey]);
    useEffect(() => () => {
        // React Aria removes modal inert state in passive cleanup. Restore
        // after that flush, not in layout cleanup before the modal releases it.
        queueMicrotask(() => restoreFocus(returnFocus));
    }, [returnFocus]);
    return <LoadBoundary resetKey={resetKey} fallback={<LoadNotice failed onClose={onClose} returnFocus={returnFocus} />}>
        <Suspense fallback={<LoadNotice label={label ?? t("common.loading")} onClose={onClose} returnFocus={returnFocus} />}>
            {children}
        </Suspense>
    </LoadBoundary>;
}

function restoreFocus(target: RefObject<HTMLElement | null>) {
    const element = target.current;
    // A disconnected trigger or one behind another modal is no longer ours
    // to focus. Do not retain a listener waiting for a later navigation/close.
    if (document.activeElement !== document.body || !element?.isConnected || element.closest("[inert]")) return;
    element.focus({ preventScroll: true });
}

class LoadBoundary extends Component<{ children: ReactNode; resetKey?: string; fallback: ReactNode }, { failed: boolean; resetKey?: string }> {
    state = { failed: false, resetKey: this.props.resetKey };
    static getDerivedStateFromError() { return { failed: true }; }
    static getDerivedStateFromProps(props: { resetKey?: string }, state: { resetKey?: string }) {
        return props.resetKey !== state.resetKey ? { failed: false, resetKey: props.resetKey } : null;
    }
    render() { return this.state.failed ? this.props.fallback : this.props.children; }
}

function LoadNotice({ label, failed = false, onClose, returnFocus }: { label?: string; failed?: boolean; onClose?: () => void; returnFocus: RefObject<HTMLElement | null> }) {
    const { t } = useI18n();
    const host = useRef<HTMLDivElement>(null);
    // This inline/toolbar notice is below real Sheets and modal dialogs.
    useCloseLayer(closeOrder.sheet - 1, () => onClose?.(), !!onClose);
    const dismissable = !!onClose;
    useLayoutEffect(() => {
        if (!dismissable) return;
        if (!returnFocus.current && document.activeElement instanceof HTMLElement) returnFocus.current = document.activeElement;
        const notice = host.current;
        notice?.querySelector("button")?.focus({ preventScroll: true });
        return () => {
            if (notice?.contains(document.activeElement)) queueMicrotask(() => restoreFocus(returnFocus));
        };
    }, [dismissable, returnFocus]);
    return <div ref={host} role={failed ? "alert" : "status"} onKeyDown={(event) => {
        // Escape belongs to the focused notice, never a document-wide
        // listener that could also dismiss a modal layered above it.
        if (onClose && event.key === "Escape" && !event.nativeEvent.isComposing && !event.defaultPrevented) {
            event.preventDefault();
            event.stopPropagation();
            onClose();
        }
    }} className="flex min-w-0 flex-wrap items-center gap-3 rounded-lg bg-primary p-4 text-sm text-secondary">
        <p className={failed ? "text-error-primary" : undefined}>{failed ? t("status.unavailable") : label}</p>
        {onClose && <Button size="sm" color="secondary" onClick={onClose}>{t("common.close")}</Button>}
    </div>;
}
