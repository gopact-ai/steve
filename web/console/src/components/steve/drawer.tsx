import { useEffect, type ReactNode } from "react";
import { X } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";

// Drawer is the right-hand sheet every page opens on one thing — a
// machine, an agent, a project, a task: a title row with its badges, a
// line under it, actions on the right, a close button, and sections.
// Escape closes it, so nothing on a page is a trap.
export function Drawer({ title, subtitle, actions, onClose, width = 560, children }: { title: ReactNode; subtitle?: ReactNode; actions?: ReactNode; onClose: () => void; width?: number; children: ReactNode }) {
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
        window.addEventListener("keydown", onKey);
        return () => window.removeEventListener("keydown", onKey);
    }, [onClose]);
    return (
        <div role="dialog" aria-modal="false" style={{ width }} className="fixed inset-y-0 right-0 z-20 flex max-w-full flex-col border-l border-secondary bg-primary shadow-xl">
            <div className="flex items-start gap-3 border-b border-secondary px-5 py-4">
                <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">{title}</div>
                    {subtitle}
                </div>
                {actions}
                <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label="关闭" />
            </div>
            <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-5 py-4 text-sm">{children}</div>
        </div>
    );
}

// DrawerSection is one heading inside a drawer, small caps, with room
// for a note on the right.
export function DrawerSection({ title, aside, children }: { title: ReactNode; aside?: ReactNode; children: ReactNode }) {
    return (
        <section>
            <h3 className="mb-2 flex items-center gap-2 text-xs font-medium uppercase tracking-wide text-quaternary">
                <span>{title}</span>
                {aside && <span className="ml-auto normal-case tracking-normal">{aside}</span>}
            </h3>
            {children}
        </section>
    );
}
