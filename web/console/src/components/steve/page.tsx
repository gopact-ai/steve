import type { ReactNode } from "react";

// One skeleton for every page, so titles, descriptions, actions and
// key-value blocks look the same wherever they appear.
//
//   PageHeader  title / one-line "what this page answers" / actions on the right / an optional row below (tabs)
//   Panel       a card with a small heading, the unit of a rail or a grid
//   KeyValue    a two-column definition list, for facts about one thing
//   Chips       a row of small monospace tokens (agent ids, capabilities)

export function PageHeader({ title, description, actions, children }: { title: string; description?: ReactNode; actions?: ReactNode; children?: ReactNode }) {
    return (
        <div className="flex flex-col gap-3 border-b border-secondary bg-primary px-8 py-5">
            <div className="flex items-start gap-6">
                <div className="min-w-0 flex-1">
                    <h1 className="text-xl font-semibold text-primary">{title}</h1>
                    {description && <p className="mt-1 max-w-4xl text-sm text-tertiary">{description}</p>}
                </div>
                {actions && <div className="flex shrink-0 items-center gap-3">{actions}</div>}
            </div>
            {children}
        </div>
    );
}

export function PageBody({ children, className }: { children: ReactNode; className?: string }) {
    return <div className={`flex flex-col gap-6 px-8 py-6 ${className ?? ""}`}>{children}</div>;
}

export function Panel({ title, badge, aside, children, className }: { title?: ReactNode; badge?: ReactNode; aside?: ReactNode; children: ReactNode; className?: string }) {
    return (
        <section className={`flex flex-col rounded-xl bg-primary shadow-xs ring-1 ring-secondary ${className ?? ""}`}>
            {title && (
                <header className="flex items-center gap-2 border-b border-secondary px-5 py-3">
                    <h2 className="shrink-0 whitespace-nowrap text-sm font-semibold text-primary">{title}</h2>
                    {badge}
                    {aside && <div className="ml-auto flex items-center gap-2">{aside}</div>}
                </header>
            )}
            <div className="flex flex-col gap-3 px-5 py-4">{children}</div>
        </section>
    );
}

export function KeyValue({ rows, dense }: { rows: { k: string; v: ReactNode; hint?: string }[]; dense?: boolean }) {
    return (
        <dl className={`grid grid-cols-[7rem_1fr] ${dense ? "gap-x-3 gap-y-1" : "gap-x-4 gap-y-2"} text-sm`}>
            {rows.map((r) => (
                <div key={r.k} className="contents">
                    <dt className="text-tertiary" title={r.hint}>{r.k}</dt>
                    <dd className="min-w-0 break-words text-primary [overflow-wrap:anywhere]">{r.v}</dd>
                </div>
            ))}
        </dl>
    );
}

export function Chips({ items, empty, tone = "default" }: { items: { id: string; title?: string }[]; empty?: ReactNode; tone?: "default" | "muted" }) {
    if (!items.length) return <span className="text-quaternary">{empty ?? "—"}</span>;
    return (
        <span className="flex flex-wrap gap-1">
            {items.map((c) => (
                <span key={c.id} title={c.title} className={`rounded-md px-1.5 py-0.5 font-mono text-xs ring-1 ring-inset ${tone === "muted" ? "text-quaternary ring-secondary" : "bg-secondary text-primary ring-secondary"}`}>{c.id}</span>
            ))}
        </span>
    );
}
