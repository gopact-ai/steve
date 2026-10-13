import { useEffect, useMemo, useRef, useState } from "react";
import { ChevronDown, ChevronRight, DotsHorizontal, Mail01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Badge } from "@/components/base/badges/badges";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { IconButton } from "@/components/steve/icon-button";
import { Drawer } from "@/components/steve/drawer";
import { MailCategoryInput } from "@/components/steve/mail-category-input";
import { Nothing } from "@/components/steve/ui";
import { useI18n } from "@/providers/locale-provider";
import { providerNames, type MailAccount } from "@/lib/mail-types";
import { when } from "@/lib/format";

type RowAction = "edit" | "login" | "sync" | "pause" | "remove" | "job";
function mailState(row: MailAccount) {
    return !row.enabled ? "paused" : row.health?.state === "error" ? "error" : row.health?.state === "syncing" ? "syncing" : row.authorized ? "ready" : "needsLogin";
}

export function MailRoster({ accounts, params, change, disabled, busy, error, onAction, onBatch }: {
    accounts: MailAccount[]; params: URLSearchParams; change: (key: string, value: string) => void; disabled: boolean; busy: string; error: string;
    onAction: (row: MailAccount, action: RowAction) => void; onBatch: (ids: string[], update: { group?: string; enabled?: boolean }) => Promise<boolean>;
}) {
    const { t } = useI18n(); const [selected, setSelected] = useState(new Set<string>()); const [moving, setMoving] = useState(false);
    const search = params.get("q") || ""; const category = params.get("category") || "";
    const provider = params.get("provider") || "all"; const status = params.get("status") || "all";
    const grouping = params.get("group_by") === "provider" ? "provider" : "category";
    const listRef = useRef<HTMLDivElement>(null);
    useEffect(() => { listRef.current?.scrollTo({ top: 0 }); }, [category, provider, status, search, grouping]);
    const categories = useMemo(() => {
        const counts = new Map<string, number>(); for (const row of accounts) counts.set(row.group || "", (counts.get(row.group || "") || 0) + 1);
        return [...counts].sort(([a], [b]) => a.localeCompare(b));
    }, [accounts]);
    const visible = useMemo(() => accounts.filter(row =>
        (!category || (category === "uncategorized" ? !row.group : row.group === category.slice(2))) &&
        (provider === "all" || row.provider === provider) && (status === "all" || mailState(row) === status) &&
        `${row.address} ${row.title} ${providerNames[row.provider]} ${row.group} ${(row.tags || []).join(" ")}`.toLocaleLowerCase().includes(search.toLocaleLowerCase()))
        .sort((a, b) => (a.title || a.address).localeCompare(b.title || b.address)), [accounts, category, provider, status, search]);
    const groups = useMemo(() => {
        const result = new Map<string, MailAccount[]>();
        for (const row of visible) { const key = grouping === "provider" ? row.provider : row.group || ""; const rows = result.get(key) || []; rows.push(row); result.set(key, rows); }
        return [...result].sort(([a], [b]) => a.localeCompare(b));
    }, [visible, grouping]);
    const collapsed = new Set((params.get("collapsed") || "").split(",").filter(Boolean));
    const chosen = visible.filter(row => selected.has(row.id)).map(row => row.id);
    const filter = (key: string, value: string) => { setSelected(new Set()); change(key, value); };
    const select = (ids: string[], checked: boolean) => setSelected(before => { const next = new Set(before); for (const id of ids) checked ? next.add(id) : next.delete(id); return next; });
    const batch = async (update: { group?: string; enabled?: boolean }) => { if (await onBatch(chosen, update)) { setSelected(new Set()); setMoving(false); if (update.group !== undefined && category) change("category", update.group ? `g:${update.group}` : "uncategorized"); } };
    const categoryLabel = (group: string) => group || t("mail.uncategorized");
    return <div className="grid h-[min(42rem,calc(100dvh-19rem))] min-h-96 min-w-0 grid-rows-[auto_minmax(0,1fr)] lg:h-[min(38rem,calc(100dvh-17rem))] lg:min-h-80 lg:grid-cols-[11rem_minmax(0,1fr)] lg:grid-rows-1">
        <nav aria-label={t("mail.categories")} className="flex min-h-0 min-w-0 flex-col overflow-hidden border-b border-secondary p-3 lg:border-r lg:border-b-0">
            <h3 className="mb-2 text-xs font-semibold text-tertiary">{t("mail.categories")}</h3>
            <div className="flex min-h-0 gap-1 overflow-auto overscroll-contain lg:flex-1 lg:flex-col">
                {[["", t("mail.allAccounts"), accounts.length], ...categories.map(([group, count]) => [group ? `g:${group}` : "uncategorized", categoryLabel(group), count])].map(([key, label, count]) =>
                    <button key={key} type="button" aria-current={category === key ? "true" : undefined} onClick={() => filter("category", String(key))} className={`flex min-w-0 shrink-0 items-center justify-between gap-3 rounded-md px-2.5 py-2 text-left text-sm outline-focus-ring focus-visible:outline-2 lg:shrink ${category === key ? "bg-secondary font-semibold text-primary" : "text-secondary hover:bg-primary_hover"}`}>
                        <span className="max-w-40 truncate" title={String(label)}>{label}</span><span className="text-xs tabular-nums text-tertiary">{count}</span>
                    </button>)}
            </div>
        </nav>
        <div className="flex min-h-0 min-w-0 flex-col">
            <div className="grid min-w-0 shrink-0 grid-cols-3 gap-3 border-b border-secondary p-3">
                <div className="col-span-3"><Input name="mail-search" type="search" label={t("mail.search")} value={search} onChange={value => filter("q", value)} autoComplete="off" placeholder={t("mail.searchPlaceholder")} /></div>
                <Select label={t("mail.groupBy")} selectedKey={grouping} onSelectionChange={key => filter("group_by", String(key))} items={[{ id: "category", label: t("mail.byCategory") }, { id: "provider", label: t("mail.byProvider") }]}>{item => <Select.Item {...item} />}</Select>
                <Select label={t("mail.provider")} selectedKey={provider} onSelectionChange={key => filter("provider", String(key))} items={[{ id: "all", label: t("mail.allProviders") }, ...Object.entries(providerNames).map(([id, label]) => ({ id, label }))]}>{item => <Select.Item {...item} />}</Select>
                <Select label={t("mail.statusFilter")} selectedKey={status} onSelectionChange={key => filter("status", String(key))} items={[{ id: "all", label: t("mail.allStates") }, ...(["needsLogin", "ready", "paused", "error", "syncing"] as const).map(id => ({ id, label: t(`mail.${id}`) }))]}>{item => <Select.Item {...item} />}</Select>
            </div>
            <div className="flex min-h-12 min-w-0 shrink-0 flex-wrap items-center gap-3 border-b border-secondary px-3 py-2">
                <Checkbox aria-label={t("mail.selectVisible")} isSelected={!!visible.length && chosen.length === visible.length} isIndeterminate={!!chosen.length && chosen.length < visible.length} isDisabled={disabled || !visible.length} onChange={checked => select(visible.map(row => row.id), checked)} />
                <span role="status" className="text-xs tabular-nums text-tertiary">{t(chosen.length ? "mail.selectedCount" : "mail.visibleCount", { count: chosen.length || visible.length, total: accounts.length })}</span>
                {chosen.length ? <><Button size="sm" color="secondary" isDisabled={disabled} onClick={() => setMoving(true)}>{t("mail.moveCategory")}</Button><Button size="sm" color="secondary" isDisabled={disabled} onClick={() => void batch({ enabled: false })}>{t("mail.pauseSelected")}</Button><Button size="sm" color="secondary" isDisabled={disabled} onClick={() => void batch({ enabled: true })}>{t("mail.resumeSelected")}</Button><Button size="sm" color="link-gray" isDisabled={disabled} onClick={() => setSelected(new Set())}>{t("mail.clearSelection")}</Button></> : null}
            </div>
            <div ref={listRef} role="region" aria-label={t("mail.list")} tabIndex={0} className="relative min-h-0 flex-1 overflow-y-auto overscroll-contain outline-focus-ring focus-visible:outline-2 focus-visible:-outline-offset-2">
            {!visible.length ? <div className="p-4"><Nothing icon={Mail01} title={t(accounts.length ? "mail.noMatch" : "mail.empty")}>{t(accounts.length ? "mail.searchHint" : "mail.emptyHint")}</Nothing>{accounts.length ? <Button size="sm" color="link-color" onClick={() => { change("resetFilters", ""); }}>{t("mail.clearFilters")}</Button> : null}</div> : groups.map(([key, rows]) => {
                const groupID = `${grouping}:${encodeURIComponent(key)}`; const closed = collapsed.has(groupID); const ids = rows.map(row => row.id);
                const count = ids.filter(id => selected.has(id)).length; const label = grouping === "provider" ? providerNames[key as MailAccount["provider"]] : categoryLabel(key);
                return <section key={groupID} aria-label={label} className="min-w-0">
                    <div className="sticky top-0 z-10 flex items-center gap-3 border-b border-secondary bg-secondary px-3 py-2">
                        <Checkbox aria-label={t("mail.selectGroup", { group: label })} isSelected={count === ids.length} isIndeterminate={count > 0 && count < ids.length} isDisabled={disabled} onChange={checked => select(ids, checked)} />
                        <button type="button" aria-expanded={!closed} onClick={() => { closed ? collapsed.delete(groupID) : collapsed.add(groupID); change("collapsed", [...collapsed].join(",")); }} className="flex min-w-0 flex-1 items-center gap-2 text-left text-xs font-semibold text-secondary outline-focus-ring focus-visible:outline-2">
                            {closed ? <ChevronRight aria-hidden="true" className="size-4 shrink-0" /> : <ChevronDown aria-hidden="true" className="size-4 shrink-0" />}<span className="truncate">{label}</span><span className="font-normal tabular-nums text-tertiary">{rows.length}</span>
                        </button>
                    </div>
                    {!closed ? <ul className="divide-y divide-secondary">{rows.map(row => <MailRosterRow key={row.id} row={row} selected={selected.has(row.id)} disabled={disabled} busy={busy === row.id} onSelect={checked => select([row.id], checked)} onAction={action => onAction(row, action)} />)}</ul> : null}
                </section>;
            })}
            </div>
        </div>
        {moving ? <MailCategoryMove groups={categories.map(([group]) => group).filter(Boolean)} count={chosen.length} busy={disabled} error={error} onClose={() => { if (!disabled) setMoving(false); }} onMove={group => void batch({ group })} /> : null}
    </div>;
}

function MailRosterRow({ row, selected, disabled, busy, onSelect, onAction }: { row: MailAccount; selected: boolean; disabled: boolean; busy: boolean; onSelect: (checked: boolean) => void; onAction: (action: RowAction) => void }) {
    const { t, locale } = useI18n(); const state = mailState(row); const locked = disabled || state === "syncing";
    return <li className={`flex min-w-0 items-center gap-3 px-3 py-2.5 ${selected ? "bg-primary_hover" : ""}`}>
        <Checkbox aria-label={t("mail.selectAccount", { address: row.address })} isSelected={selected} isDisabled={disabled} onChange={onSelect} />
        <div className="min-w-0 flex-1">
            <h3 className="truncate text-sm font-medium text-primary" title={row.title}>{row.title}</h3>
            <div className="flex min-w-0 items-center gap-2 text-xs text-tertiary"><span className="truncate" translate="no" title={row.address}>{row.title !== row.address ? row.address : providerNames[row.provider]}</span>{row.title !== row.address ? <span className="hidden shrink-0 sm:inline" translate="no">{providerNames[row.provider]}</span> : null}{row.tags?.length ? <span className="hidden truncate md:inline" title={row.tags.join(", ")}>· {row.tags.join(" · ")}</span> : null}</div>
            <span className="mt-1 inline-flex sm:hidden"><Badge type="pill-color" size="sm" color={state === "ready" ? "success" : state === "error" ? "error" : state === "needsLogin" ? "warning" : "gray"}>{t(`mail.${state}`)}</Badge></span>
            {row.health ? <p className="truncate text-xs text-tertiary" title={row.health.hint || row.health.code}>{t("mail.lastSync")} · {when(row.health.at, locale)}{row.health.hint ? ` · ${row.health.hint}` : row.health.code ? ` · ${row.health.code}` : ""}</p> : null}
        </div>
        <div className="hidden shrink-0 flex-col items-end gap-1 sm:flex"><Badge type="pill-color" size="sm" color={state === "ready" ? "success" : state === "error" ? "error" : state === "needsLogin" ? "warning" : "gray"}>{t(`mail.${state}`)}</Badge>{row.pending ? <span className="text-xs tabular-nums text-tertiary">{t("mail.pending", { count: row.pending })}</span> : null}</div>
        <Button size="sm" color="secondary" isDisabled={locked || (row.authorized && !row.enabled)} isLoading={busy} onClick={() => onAction(row.authorized ? "sync" : "login")}>{t(row.authorized ? "mail.sync" : "mail.login")}</Button>
        <Dropdown.Root><IconButton label={t("mail.accountActions", { address: row.address })} isDisabled={locked} icon={DotsHorizontal} /><Dropdown.Popover><Dropdown.Menu aria-label={t("mail.accountActions", { address: row.address })} onAction={key => onAction(String(key) as RowAction)}>
            <Dropdown.Item id="edit" label={t("mail.edit")} /><Dropdown.Item id="pause" label={t(row.enabled ? "mail.pause" : "mail.resume")} />
            {row.authorized ? <Dropdown.Item id="login" label={t("mail.reconnect")} /> : null}{row.login ? <Dropdown.Item id="job" label={t("mail.loginStatus")} /> : null}
            <Dropdown.Separator /><Dropdown.Item id="remove" label={t("mail.remove")} />
        </Dropdown.Menu></Dropdown.Popover></Dropdown.Root>
    </li>;
}

function MailCategoryMove({ groups, count, busy, error, onClose, onMove }: { groups: string[]; count: number; busy: boolean; error: string; onClose: () => void; onMove: (group: string) => void }) {
    const { t } = useI18n(); const [group, setGroup] = useState("");
    return <Drawer title={t("mail.moveCategory")} subtitle={t("mail.selectedCount", { count })} onClose={onClose} closeDisabled={busy}>
        <form className="flex flex-col gap-4" onSubmit={event => { event.preventDefault(); onMove(group); }}>
            <MailCategoryInput value={group} onChange={setGroup} groups={groups} disabled={busy} />
            {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
            <Button size="sm" type="submit" isLoading={busy}>{t("mail.moveSelected")}</Button>
        </form>
    </Drawer>;
}
