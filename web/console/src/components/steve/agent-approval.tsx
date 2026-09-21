import { Button } from "@/components/base/buttons/button";
import { Select } from "@/components/base/select/select";
import { DrawerSection } from "@/components/steve/drawer";
import type { Agent, Selector } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";

export const isApprovalSelector = (selector: Selector) => selector.category === "mode" || selector.id === "mode";
const approvalNames = { ask: "fleet.approval.ask", auto: "fleet.approval.auto", full: "fleet.approval.full" } as const;

// Use the tool's actual selector, just as the runtime does. An unobserved
// capability is not permission to invent modes or silently drop saved pins.
export function AgentApproval({ agent, options, editing, contextChanged, disabled, onEdit, onChange }: {
    agent: Agent;
    options: Record<string, string>;
    editing: boolean;
    contextChanged: boolean;
    disabled: boolean;
    onEdit: () => void;
    onChange: (options: Record<string, string>) => void;
}) {
    const { t } = useI18n();
    const selector = agent.selectors?.find(isApprovalSelector);
    const id = selector?.id || "mode";
    const pinned = (editing ? options : agent.options)?.[id];
    const key = agent.approval && approvalNames[agent.approval as keyof typeof approvalNames];
    const inherited = key ? t(key) : t("fleet.approvalToolDefault");
    const choices = selector?.choices || [];
    const items = [
        { id: "__none", label: t("fleet.followApproval", { value: inherited }) },
        ...choices.map(value => ({ id: value, label: value })),
    ];
    if (pinned && !choices.includes(pinned)) items.push({ id: pinned, label: t("fleet.approvalSavedMode", { value: pinned }) });
    return <DrawerSection title={t("fleet.defaultApproval")}>
        <div className="flex min-w-0 flex-col gap-3">
            {editing ? <Select size="sm" aria-label={t("fleet.defaultApproval")}
                selectedKey={pinned || "__none"} items={items}
                isDisabled={disabled || contextChanged || choices.length === 0}
                onSelectionChange={value => {
                    if (value == null) return;
                    const next = { ...options };
                    if (value === "__none") delete next[id]; else next[id] = String(value);
                    onChange(next);
                }}>
                {item => <Select.Item id={item.id}>{item.label}</Select.Item>}
            </Select> : <p className="text-sm text-secondary [overflow-wrap:anywhere]">
                {pinned ? t("fleet.approvalOverride", { value: pinned }) : t("fleet.followsApproval", { value: inherited })}
            </p>}
            <p className="text-xs text-tertiary">{t("fleet.approvalDefaultHint")}</p>
            {(contextChanged || choices.length === 0) && <p className="text-xs text-tertiary">
                {t(contextChanged ? "fleet.approvalContextChanged" : "fleet.approvalUnavailable")}
            </p>}
            <div className="flex flex-wrap items-center gap-3">
                {!editing && <Button size="sm" color="secondary" onClick={onEdit}>{t("fleet.editApproval")}</Button>}
                <a className="text-sm text-brand-secondary underline underline-offset-4" href="#/settings?section=approval">{t("fleet.globalApprovalSettings")}</a>
            </div>
        </div>
    </DrawerSection>;
}
