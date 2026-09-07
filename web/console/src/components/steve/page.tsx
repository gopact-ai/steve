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
        <header className="workbench-page-header flex min-w-0 flex-col gap-4 border-b border-secondary px-4 py-5 sm:px-6 lg:px-8">
            <div className="flex min-w-0 flex-wrap items-start gap-x-6 gap-y-3">
                <div className="min-w-0 flex-1">
                    <h1 className="text-xl font-semibold tracking-tight text-primary">{title}</h1>
                    {description && <p className="mt-1 max-w-3xl text-sm text-tertiary">{description}</p>}
                </div>
                {actions && <div className="workbench-page-actions flex max-w-full flex-wrap items-center gap-2">{actions}</div>}
            </div>
            {children}
        </header>
    );
}

export function PageBody({ children, className }: { children: ReactNode; className?: string }) {
    return <div className={`workbench-page-body flex min-w-0 flex-col gap-5 px-4 py-5 sm:px-6 lg:px-8 ${className ?? ""}`}>{children}</div>;
}

export function Panel({ title, description, badge, aside, children, className }: { title?: ReactNode; description?: ReactNode; badge?: ReactNode; aside?: ReactNode; children: ReactNode; className?: string }) {
    return (
        <section className={`workbench-panel flex min-w-0 flex-col rounded-lg bg-primary ring-1 ring-secondary ${className ?? ""}`}>
            {title && (
                <header className="workbench-panel-header flex min-w-0 flex-wrap items-start gap-x-3 gap-y-2 border-b border-secondary px-4 py-3">
                    <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-2"><h2 className="text-sm font-semibold text-primary">{title}</h2>{badge}</div>
                        {description && <p className="mt-1 text-xs text-tertiary">{description}</p>}
                    </div>
                    {aside && <div className="flex max-w-full flex-wrap items-center gap-2">{aside}</div>}
                </header>
            )}
            <div className="flex min-w-0 flex-col gap-3 px-4 py-4">{children}</div>
        </section>
    );
}

export function KeyValue({ rows, dense }: { rows: { k: string; v: ReactNode; hint?: string }[]; dense?: boolean }) {
    return (
        <dl className={`grid min-w-0 grid-cols-[minmax(5rem,7rem)_minmax(0,1fr)] ${dense ? "gap-x-3 gap-y-1.5" : "gap-x-4 gap-y-2"} text-sm`}>
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
                <span key={c.id} title={c.title} className={`max-w-full break-all rounded px-1.5 py-0.5 font-mono text-xs ${tone === "muted" ? "text-tertiary" : "bg-secondary text-secondary"}`}>{c.id}</span>
            ))}
        </span>
    );
}
