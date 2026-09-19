import { useState, useSyncExternalStore } from "react";
import { ChevronLeft, ChevronRight } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Select } from "@/components/base/select/select";
import { number } from "@/lib/format";
import { useI18n } from "@/providers/locale-provider";

// A table of forty replicas is a wall, and the reader wanted the last
// few rows. Long tables are read a page at a time, with the page size
// the reader's own choice — kept for every table, because someone who
// wants fifty rows of one list wants fifty of the next.
export const PAGE_SIZE = "steve.table.pageSize";
const sizes = [10, 20, 50, 100];
const fallback = 20;

function stored(): number {
    try {
        const saved = Number(localStorage.getItem(PAGE_SIZE));
        return sizes.includes(saved) ? saved : fallback;
    } catch { return fallback; }
}

let size = stored();
const listeners = new Set<() => void>();
const subscribe = (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener); }; };

export function setPageSize(next: number) {
    if (!sizes.includes(next) || next === size) return;
    size = next;
    try { localStorage.setItem(PAGE_SIZE, String(next)); } catch { /* ignore */ }
    for (const listener of [...listeners]) listener();
}

export const usePageSize = () => useSyncExternalStore(subscribe, () => size);

// usePaged returns the rows of the current page and the footer that
// moves between them. A table short enough to read whole gets no footer:
// the control would be more furniture than the rows it governs.
export function usePaged<T>(items: T[]): { items: T[]; footer: React.ReactNode } {
    const { t, locale } = useI18n();
    const perPage = usePageSize();
    const [asked, setPage] = useState(0);
    const pages = Math.max(1, Math.ceil(items.length / perPage));
    const page = Math.min(asked, pages - 1);
    const from = page * perPage;
    const shown = items.slice(from, from + perPage);
    if (items.length <= sizes[0]) return { items, footer: null };
    return {
        items: shown,
        footer: (
            <div className="table-pager">
                <span className="table-pager-count">{t("table.range", { from: number(from + 1, locale), to: number(from + shown.length, locale), total: number(items.length, locale) })}</span>
                <div className="table-pager-controls">
                    <Select size="sm" aria-label={t("table.pageSize")} selectedKey={String(perPage)} onSelectionChange={(key) => { if (key) { setPage(0); setPageSize(Number(key)); } }}
                        items={sizes.map((n) => ({ id: String(n), label: t("table.perPage", { count: number(n, locale) }) }))}>
                        {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                    </Select>
                    <span className="table-pager-page">{t("table.page", { page: number(page + 1, locale), pages: number(pages, locale) })}</span>
                    <Button size="sm" color="tertiary" iconLeading={ChevronLeft} aria-label={t("table.previous")} isDisabled={page === 0} onClick={() => setPage(page - 1)} />
                    <Button size="sm" color="tertiary" iconLeading={ChevronRight} aria-label={t("table.next")} isDisabled={page >= pages - 1} onClick={() => setPage(page + 1)} />
                </div>
            </div>
        ),
    };
}
