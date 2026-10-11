import { useState } from "react";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Button } from "@/components/base/buttons/button";
import { Drawer, DrawerSection } from "@/components/steve/drawer";
import { useI18n } from "@/providers/locale-provider";
import type { Connection } from "@/lib/assistant-types";

// Reuses the workbench's forms and focus-managed drawer. Creating a grant is
// distinct from a provider advertising availability or publishing an event.
export function ConnectionEditor({ projects, busy, error, onClose, onSubmit }: {
    projects: string[]; busy: boolean; error: string; onClose: () => void; onSubmit: (body: Partial<Connection>) => void;
}) {
    const { t } = useI18n();
    const [key] = useState(() => crypto.randomUUID());
    const [name, setName] = useState("");
    const [kind, setKind] = useState<"provider" | "client">("provider");
    const [events, setEvents] = useState("mail.received");
    const [intelligence, setIntelligence] = useState(false);
    const [conversation, setConversation] = useState("console:main");
    const [project, setProject] = useState(projects[0] || "");
    const [capability, setCapability] = useState("notifications.show");
    const [approval, setApproval] = useState(true);
    return <Drawer title={t("assistant.enroll")} onClose={onClose} closeDisabled={busy}>
        <form className="flex min-w-0 flex-col gap-5" onSubmit={(event) => {
            event.preventDefault();
            onSubmit({ idempotency_key: key, name, kind, event_types: kind === "provider" ? events.split(",").map((type) => type.trim()).filter(Boolean) : [], work: intelligence ? { [conversation]: project } : {}, capabilities: kind === "client" ? [{ client: "", capability, conversation, approval }] : [] });
        }}>
            <DrawerSection title={t("assistant.connections")}>
                <div className="flex flex-col gap-3"><Input name="connection-name" label={t("assistant.name")} value={name} onChange={setName} autoComplete="off" isRequired isDisabled={busy} />
                <Select label={t("assistant.kind")} selectedKey={kind} onSelectionChange={(key) => setKind(key as typeof kind)} isDisabled={busy} items={[{ id: "provider", label: t("assistant.provider") }, { id: "client", label: t("assistant.client") }]}>{(item) => <Select.Item {...item} />}</Select>
                {kind === "provider" ? <Input name="event-types" label={t("assistant.eventTypes")} hint={t("assistant.eventTypesHint")} value={events} onChange={setEvents} autoComplete="off" spellCheck="false" isRequired isDisabled={busy} /> : <><Input name="capability" label={t("assistant.capability")} value={capability} onChange={setCapability} autoComplete="off" spellCheck="false" isRequired isDisabled={busy} /><Checkbox label={t("assistant.clientApproval")} isSelected={approval} onChange={setApproval} isDisabled={busy} /></>}
                <Checkbox label={t("assistant.intelligence")} isSelected={intelligence} onChange={setIntelligence} isDisabled={busy} /></div>
            </DrawerSection>
            {intelligence || kind === "client" ? <DrawerSection title={t("assistant.grantScope")}><div className="flex flex-col gap-3">
                <Input name="conversation" label={t("assistant.conversation")} value={conversation} onChange={setConversation} autoComplete="off" spellCheck="false" isRequired isDisabled={busy} />
                {intelligence ? <Select label={t("assistant.project")} placeholder={t("assistant.selectProject")} selectedKey={project || null} onSelectionChange={(key) => setProject(String(key))} items={projects.map((id) => ({ id, label: id }))} isRequired isDisabled={busy}>{(item) => <Select.Item {...item} />}</Select> : null}
                <p className="text-xs text-tertiary">{t("assistant.scopeHint")}</p>
            </div></DrawerSection> : null}
            {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
            <div className="flex flex-wrap justify-end gap-2"><Button size="sm" color="secondary" isDisabled={busy} onClick={onClose}>{t("assistant.close")}</Button><Button type="submit" size="sm" isLoading={busy}>{t("assistant.enroll")}</Button></div>
        </form>
    </Drawer>;
}
