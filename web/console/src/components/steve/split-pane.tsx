import { IconButton } from "@/components/steve/icon-button";
import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ChevronRight, GitBranch01, MessageChatSquare, X } from "@untitledui/icons";
import { useI18n } from "@/providers/locale-provider";
import { PaneResizer } from "./pane-resizer";
import "@/styles/split-pane.css";

// The workbench splits the way a terminal does: the conversation keeps
// the left, and whatever is worth reading beside it — a side chat, a
// delegated child — is a tab on the right, behind a divider the reader
// owns. Nothing here knows what a tab contains; the page that opens one
// says how to draw it, so this pane never reaches back into a page.

export type SplitKind = "chat" | "delegation";
export interface SplitTab { id: string; kind: SplitKind; task?: string }

interface SplitValue {
    tabs: SplitTab[];
    active: string;
    hidden: boolean;
    open: (tab: SplitTab) => void;
    close: (id: string) => void;
    closeKind: (kind: SplitKind) => void;
    focus: (id: string) => void;
    setHidden: (hidden: boolean) => void;
}

const Context = createContext<SplitValue | null>(null);

/** useSplitPane is null wherever no pane is mounted, so a card that can
 *  open one stays usable in a transcript that has nowhere to put it. */
export const useSplitPane = () => useContext(Context);

export function SplitPaneProvider({ children }: { children: ReactNode }) {
    const [tabs, setTabs] = useState<SplitTab[]>([]);
    const [active, setActive] = useState("");
    const [hidden, setHidden] = useState(false);
    const open = useCallback((tab: SplitTab) => {
        setTabs((list) => list.some((item) => item.id === tab.id) ? list.map((item) => item.id === tab.id ? { ...item, ...tab } : item) : [...list, tab]);
        setActive(tab.id);
        setHidden(false);
    }, []);
    const close = useCallback((id: string) => {
        setTabs((list) => {
            const index = list.findIndex((item) => item.id === id);
            if (index < 0) return list;
            const next = list.filter((item) => item.id !== id);
            // Closing the tab in front hands the pane to its neighbour,
            // the way closing a window leaves the one beside it in view.
            setActive((current) => current !== id ? current : (next[index] || next[index - 1] || { id: "" }).id);
            return next;
        });
    }, []);
    const closeKind = useCallback((kind: SplitKind) => {
        setTabs((list) => {
            const next = list.filter((item) => item.kind !== kind);
            if (next.length === list.length) return list;
            setActive((current) => next.some((item) => item.id === current) ? current : (next[next.length - 1]?.id || ""));
            return next;
        });
    }, []);
    const focus = useCallback((id: string) => { setActive(id); setHidden(false); }, []);
    const value = useMemo<SplitValue>(() => ({ tabs, active: tabs.some((item) => item.id === active) ? active : (tabs[tabs.length - 1]?.id || ""), hidden, open, close, closeKind, focus, setHidden }), [tabs, active, hidden, open, close, closeKind, focus]);
    return <Context.Provider value={value}>{children}</Context.Provider>;
}

const storageKey = "steve.split.width";
const defaultWidth = 420;
const minWidth = 320;
const widthLimit = 760;
const conversationMinimum = 480;

function readWidth() {
    try {
        const stored = Number(localStorage.getItem(storageKey));
        if (Number.isFinite(stored) && stored >= minWidth) return Math.min(stored, widthLimit);
    } catch { /* Use the default when browser storage is unavailable. */ }
    return defaultWidth;
}

export function SplitPane({ tabs, active, label, onFocus, onClose, onHide, render }: {
    tabs: SplitTab[];
    active: string;
    /** A tab's name is read, not stored, so it follows the language. */
    label: (tab: SplitTab) => string;
    onFocus: (id: string) => void;
    onClose: (tab: SplitTab) => void;
    onHide: () => void;
    render: (tab: SplitTab) => ReactNode;
}) {
    const { t } = useI18n();
    const panel = useRef<HTMLDivElement>(null);
    const [preferred, setPreferred] = useState(readWidth);
    const [maximum, setMaximum] = useState(widthLimit);
    const width = Math.round(Math.max(minWidth, Math.min(preferred, maximum)));
    const current = tabs.find((tab) => tab.id === active) || tabs[tabs.length - 1];

    // The pane may only take room the conversation can spare. What the
    // two of them hold together is fixed while the divider moves, so the
    // conversation's own width is the honest measure of what is left.
    useLayoutEffect(() => {
        const own = panel.current;
        const beside = own?.parentElement?.querySelector<HTMLElement>(".conversation-content");
        if (!own || !beside) return;
        const measure = () => setMaximum(Math.max(minWidth, Math.min(widthLimit, beside.clientWidth + own.clientWidth - conversationMinimum)));
        measure();
        const observer = new ResizeObserver(measure);
        observer.observe(beside);
        return () => observer.disconnect();
    }, []);

    const change = useCallback((value: number) => {
        setPreferred((last) => {
            const next = Math.round(Math.max(minWidth, Math.min(maximum, value)));
            if (next !== last) { try { localStorage.setItem(storageKey, String(next)); } catch { /* Keep the width for this open pane. */ } }
            return next;
        });
    }, [maximum]);

    if (!current) return null;
    return (
        <aside ref={panel} className="split-pane" style={{ width }} aria-label={t("split.region")}>
            <PaneResizer edge="start" width={width} onChange={change} min={minWidth} max={maximum} initial={defaultWidth} label={t("split.resize")} />
            <div className="split-pane-bar">
                <div role="tablist" aria-label={t("split.region")} className="split-pane-tabs">
                    {tabs.map((tab) => {
                        const name = label(tab);
                        const Icon = tab.kind === "chat" ? MessageChatSquare : GitBranch01;
                        return <span key={tab.id} className="split-tab" data-active={tab.id === current.id || undefined}>
                            <button type="button" role="tab" id={`split-tab-${tab.id}`} aria-selected={tab.id === current.id} aria-controls={`split-panel-${tab.id}`} tabIndex={tab.id === current.id ? 0 : -1} className="split-tab-label" title={name} onClick={() => onFocus(tab.id)}>
                                <Icon className="split-tab-icon" aria-hidden="true" />
                                <span className="split-tab-name">{name}</span>
                            </button>
                            <button type="button" className="split-tab-close" aria-label={t("split.closeTab", { title: name })} onClick={() => onClose(tab)}><X aria-hidden="true" /></button>
                        </span>;
                    })}
                </div>
                <IconButton className="split-pane-hide shrink-0" label={t("split.hide")} title={t("split.hide")} onClick={onHide} icon={ChevronRight} />
            </div>
            <div key={current.id} id={`split-panel-${current.id}`} role="tabpanel" aria-labelledby={`split-tab-${current.id}`} className="split-pane-body">{render(current)}</div>
        </aside>
    );
}
