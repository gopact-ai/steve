import { useState } from "react";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Button } from "@/components/base/buttons/button";
import { Drawer, DrawerSection } from "@/components/steve/drawer";
import { useI18n } from "@/providers/locale-provider";
import type { Connection, Subscription } from "@/lib/assistant-types";

export function SubscriptionEditor({ initial, projects, providers, busy, error, onClose, onSubmit }: {
    initial: Subscription; projects: string[]; providers: Connection[]; busy: boolean; error: string;
    onClose: () => void; onSubmit: (body: Subscription) => void;
}) {
    const { t } = useI18n(); const [draft, setDraft] = useState(initial);
    const change = (patch: Partial<Subscription>) => setDraft((before) => ({ ...before, ...patch }));
    return <Drawer title={initial.revision ? t("assistant.edit") : t("assistant.createRule")} subtitle={draft.id} onClose={onClose} closeDisabled={busy}>
        <form className="flex min-w-0 flex-col gap-5" onSubmit={(event) => { event.preventDefault(); onSubmit(draft); }}>
            <DrawerSection title={t("assistant.subscriptions")}><div className="flex flex-col gap-3">
                <Input name="rule-name" label={t("assistant.name")} value={draft.name} onChange={(name) => change({ name })} isRequired autoComplete="off" isDisabled={busy} />
                <Input name="event-type" label={t("assistant.eventType")} value={draft.event_type} onChange={(event_type) => change({ event_type })} isRequired autoComplete="off" spellCheck="false" isDisabled={busy} />
                <Input name="source" label={t("assistant.source")} hint={t("assistant.sourceHint")} value={draft.source || ""} onChange={(source) => change({ source })} autoComplete="off" spellCheck="false" isDisabled={busy} />
                <Select label={t("assistant.mode")} selectedKey={draft.mode} onSelectionChange={(key) => change({ mode: key as Subscription["mode"] })} isDisabled={busy} items={[{ id: "inbox", label: t("assistant.manual") }, { id: "work", label: t("assistant.work") }, { id: "provider", label: t("assistant.provider") }]}>{(item) => <Select.Item {...item} />}</Select>
                <Checkbox label={t("assistant.enabled")} isSelected={draft.enabled} onChange={(enabled) => change({ enabled })} isDisabled={busy} />
            </div></DrawerSection>
            {draft.mode === "work" ? <DrawerSection title={t("assistant.work")}><div className="flex flex-col gap-3">
                <Input name="conversation" label={t("assistant.conversation")} value={draft.conversation || "console:main"} onChange={(conversation) => change({ conversation })} isRequired autoComplete="off" spellCheck="false" isDisabled={busy} />
                <Select label={t("assistant.project")} placeholder={t("assistant.selectProject")} selectedKey={draft.project || null} onSelectionChange={(key) => change({ project: String(key) })} items={projects.map((id) => ({ id, label: id }))} isRequired isDisabled={busy}>{(item) => <Select.Item {...item} />}</Select>
                <p className="text-xs text-tertiary">{t("assistant.scopeHint")}</p>
                <TextArea name="instruction" label={t("assistant.instruction")} hint={t("assistant.instructionHint")} value={draft.instruction || ""} onChange={(instruction) => change({ instruction })} isRequired isDisabled={busy} />
            </div></DrawerSection> : draft.mode === "provider" ? <DrawerSection title={t("assistant.handler")}><div className="flex flex-col gap-3">
                <Select label={t("assistant.handler")} placeholder={t("assistant.selectHandler")} selectedKey={draft.handler || null} onSelectionChange={(key) => change({ handler: String(key) })} items={providers.filter((item) => item.enabled && item.kind === "provider").map((item) => ({ id: item.id, label: item.name }))} isRequired isDisabled={busy}>{(item) => <Select.Item {...item} />}</Select>
                <Checkbox label={t("assistant.replay")} hint={t("assistant.replayHint")} isSelected={draft.replay_safe} onChange={(replay_safe) => change({ replay_safe })} isDisabled={busy} />
            </div></DrawerSection> : null}
            <Checkbox label={t("assistant.derived")} hint={t("assistant.derivedHint")} isSelected={draft.include_derived} onChange={(include_derived) => change({ include_derived })} isDisabled={busy} />
            {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
            <div className="flex flex-wrap justify-end gap-2"><Button size="sm" color="secondary" isDisabled={busy} onClick={onClose}>{t("assistant.close")}</Button><Button type="submit" size="sm" isLoading={busy}>{t("assistant.save")}</Button></div>
        </form>
    </Drawer>;
}
