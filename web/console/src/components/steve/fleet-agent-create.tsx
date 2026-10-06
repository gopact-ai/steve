import { useEffect, useRef, useState } from "react";
import { Plus, Trash01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { IconButton } from "@/components/steve/icon-button";
import { useI18n } from "@/providers/locale-provider";
import { addAgent, fetchNodeSettings, fetchState, saveNodeSettings, type HarnessSetting, type NodeSettings } from "@/lib/api/fleet";
import { bindFleetAgent, configuredHarnesses, createFleetAgent, FleetAgentCreateError, fleetWriteRejected, inspectFleetAgent, type FleetAgentPorts, type FleetAgentReceipt } from "@/lib/api/fleet-agent-create";
import { HTTPError, message } from "@/lib/http";

type Machine = { id: string; label: string; supportingText?: string };
const customChoice = "__custom_command";
const nameShape = /^[a-z0-9][a-z0-9._-]{0,63}$/;
function receiptKey() { return `steve.fleet-agent.receipt:${new URL(".", window.location.href).href}`; }
function readReceipt(): FleetAgentReceipt | null {
    const raw = localStorage.getItem(receiptKey());
    if (!raw) return null;
    const value = JSON.parse(raw) as FleetAgentReceipt;
    if (!value || ![value.id, value.node, value.harness, value.revision, value.launch].every(item => typeof item === "string" && item.length > 0) || !nameShape.test(value.id) || !/^[a-f0-9]{64}$/.test(value.launch) || !["save-unknown", "save-rejected", "saved", "bind-unknown", "bind-rejected", "bound"].includes(value.phase)) throw new FleetAgentCreateError("invalidReceipt");
    return value;
}

// The node's settings are the sole launch registry. This form keeps only an
// operation receipt locally, never a second registry or raw environment values.
export function FleetAgentCreate({ hub, machines, initialNode, onChanged, onBusyChange }: { hub: string; machines: Machine[]; initialNode?: string; onChanged: () => void; onBusyChange: (busy: boolean) => void }) {
    const { t } = useI18n();
    const ports: FleetAgentPorts = {
        readSettings: fetchNodeSettings, saveSettings: saveNodeSettings, readAgents: fetchState,
        bind: ({ id, harness, node }) => addAgent({ id, harness, node: node === hub ? undefined : node }), rejected: (error, step) => error instanceof HTTPError && fleetWriteRejected(error.status, error.message, step),
    };
    const [recovery] = useState(() => { try { return { receipt: readReceipt(), error: "" }; } catch (error) { return { receipt: null, error: error instanceof FleetAgentCreateError ? t(`fleet.commandError.${error.code}`) : t("fleet.commandStorageError") }; } });
    const [receipt, setReceipt] = useState(recovery.receipt);
    const [node, setNode] = useState(recovery.receipt?.node || initialNode || hub);
    const [id, setID] = useState(recovery.receipt?.id || "");
    const [choice, setChoice] = useState(recovery.receipt?.harness || "");
    const [harnessID, setHarnessID] = useState("");
    const [command, setCommand] = useState("");
    const [args, setArgs] = useState<string[]>([]);
    const [env, setEnv] = useState<string[]>([]);
    const [settings, setSettings] = useState<NodeSettings | null>(null);
    const [readError, setReadError] = useState("");
    const [error, setError] = useState(recovery.error);
    const [busy, setBusy] = useState(false);
    const [readVersion, setReadVersion] = useState(0);
    const acting = useRef(false);
    const latestReceipt = useRef(receipt);
    useEffect(() => {
        let alive = true;
        const controller = new AbortController();
        setSettings(null); setReadError("");
        void fetchNodeSettings(node, controller.signal).then(({ settings }) => {
            if (!alive) return;
            if (!settings?.revision || !settings.harnesses) throw new FleetAgentCreateError("invalidReceipt");
            setSettings(settings);
        }).catch(error => { if (alive) setReadError(message(error)); });
        return () => { alive = false; controller.abort(); };
    }, [node, readVersion]);
    function remember(value: FleetAgentReceipt) {
        if (JSON.stringify(readReceipt()) !== JSON.stringify(latestReceipt.current)) throw new Error(t("fleet.commandReceiptChanged"));
        try { localStorage.setItem(receiptKey(), JSON.stringify(value)); }
        catch { throw new Error(t("fleet.commandStorageError")); }
        latestReceipt.current = value; setReceipt(value);
        if (value.phase === "bound") localStorage.removeItem(receiptKey());
    }
    function reason(error: unknown) {
        if (error instanceof FleetAgentCreateError) return t(`fleet.commandError.${error.code}`);
        return message(error);
    }
    async function act(inspect = false) {
        if (acting.current || receipt?.phase === "bound" || recovery.error) return;
        acting.current = true; setBusy(true); onBusyChange(true); setError("");
        try {
            if (JSON.stringify(readReceipt()) !== JSON.stringify(latestReceipt.current)) throw new Error(t("fleet.commandReceiptChanged"));
            const pending = latestReceipt.current;
            if (pending) {
                const observed = await inspectFleetAgent(pending, ports);
                if (!observed) {
                    localStorage.removeItem(receiptKey()); latestReceipt.current = null; setReceipt(null);
                    setReadVersion(version => version + 1); return;
                }
                remember(observed);
                if (observed.phase === "bound") { onChanged(); return; }
                if (inspect || (observed.phase !== "saved" && observed.phase !== "bind-rejected")) return;
                await bindFleetAgent(observed, ports, remember);
            } else {
                if (!settings) return;
                const custom: HarnessSetting | undefined = choice === customChoice ? { command, args, env } : undefined;
                await createFleetAgent({ id: id.trim().toLowerCase(), harness: custom ? harnessID.trim() : choice, node }, settings, custom, ports, remember);
            }
            onChanged();
        } catch (error) { setError(reason(error)); }
        finally { acting.current = false; setBusy(false); onBusyChange(false); }
    }
    const custom = choice === customChoice;
    const harness = custom ? { command, args } : settings?.harnesses[choice];
    const choices = settings ? configuredHarnesses(settings) : [];
    const selected = choices.includes(choice) || custom ? choice : null;
    const machine = machines.find(machine => (machine.id === "__hub" ? hub : machine.id) === node)?.label || node;
    const locked = busy || !!receipt || !!recovery.error;
    const secure = !!globalThis.crypto?.subtle;
    const valid = secure && !!settings && nameShape.test(id.trim().toLowerCase()) && (custom ? nameShape.test(harnessID.trim()) && !!command.trim() && !Object.hasOwn(settings.harnesses, harnessID.trim()) : choices.includes(choice));
    const saved = receipt && ["saved", "bind-unknown", "bind-rejected", "bound"].includes(receipt.phase);
    const unknown = receipt?.phase === "save-unknown" || receipt?.phase === "bind-unknown";
    const bound = receipt?.phase === "bound";
    return <div className="flex min-w-0 flex-col gap-4">
        {!bound && <fieldset disabled={locked} className="flex min-w-0 flex-col gap-4">
            <legend className="mb-3 text-sm font-semibold text-primary">{t("fleet.commandBinding")}</legend>
            <Input size="sm" label={t("fleet.name")} name="fleet-agent-name" maxLength={64} autoComplete="off" spellCheck="false" placeholder="reviewer" value={id} onChange={setID} isInvalid={!!id && !nameShape.test(id.trim().toLowerCase())} hint={t("fleet.commandNameHint")} isDisabled={locked} />
            <Select size="sm" label={t("fleet.machine")} hint={t("fleet.chooseMachineHint")} selectedKey={node === hub ? "__hub" : node} isDisabled={locked} onSelectionChange={key => { if (key) { setNode(String(key) === "__hub" ? hub : String(key)); setChoice(""); setError(""); } }} items={machines}>
                {item => <Select.Item id={item.id} supportingText={item.supportingText}>{item.label}</Select.Item>}
            </Select>
            <Select size="sm" label={t("fleet.commandChoice")} placeholder={t("fleet.commandChoose")} hint={t("fleet.commandChoicesHint")} selectedKey={selected} isDisabled={locked || !settings} onSelectionChange={key => { if (key) { setChoice(String(key)); setError(""); } }} items={[...choices.map(id => ({ id, label: id })), { id: customChoice, label: t("fleet.customCommand") }]}>
                {item => <Select.Item id={item.id}>{item.label}</Select.Item>}
            </Select>
            {custom && <section aria-label={t("fleet.commandLaunch")} className="flex min-w-0 flex-col gap-3 rounded-lg border border-secondary p-3">
                <h3 className="text-sm font-medium text-primary">{t("fleet.commandLaunch")}</h3>
                <Input size="sm" label={t("fleet.commandHarnessID")} name="fleet-agent-harness" maxLength={64} autoComplete="off" spellCheck="false" placeholder="my-acp" value={harnessID} onChange={setHarnessID} isInvalid={!!harnessID && !nameShape.test(harnessID.trim())} isDisabled={locked} hint={t("fleet.commandHarnessHint")} />
                {settings && Object.hasOwn(settings.harnesses, harnessID.trim()) && <p role="alert" className="text-xs text-error-primary">{t("fleet.commandError.harnessExists")}</p>}
                <Input size="sm" label={t("fleet.commandExecutable")} name="fleet-agent-executable" autoComplete="off" spellCheck="false" placeholder="/path/to/my-agent" value={command} onChange={setCommand} isDisabled={locked} hint={t("fleet.commandNoShell")} />
                <StringRows values={args} onChange={setArgs} disabled={locked} kind="argument" />
                <details className="min-w-0 text-xs text-tertiary"><summary className="min-h-10 cursor-pointer py-2 focus-visible:outline-2 focus-visible:outline-focus-ring">{t("fleet.commandAdvancedEnv")}</summary>
                    <p className="mb-3 leading-5 text-warning-primary">{t("fleet.commandEnvWarning")}</p>
                    <StringRows values={env} onChange={setEnv} disabled={locked} kind="environment" />
                </details>
            </section>}
        </fieldset>}
        {!settings && !readError && <p role="status" className="text-sm text-tertiary">{t("fleet.commandLoading")}</p>}
        {readError && <div role="alert" className="flex flex-col gap-2 text-sm text-error-primary"><p>{readError}</p><Button size="sm" color="secondary" isDisabled={busy} onClick={() => setReadVersion(version => version + 1)}>{t("fleet.commandReload")}</Button></div>}
        {harness && <section aria-label={t("fleet.commandPreview")} className="min-w-0 space-y-2 rounded-lg bg-secondary p-3">
            <h3 className="text-sm font-medium text-primary">{t("fleet.commandPreview")}</h3>
            <p className="break-all text-xs text-secondary">{machine} · {receipt?.harness || (custom ? harnessID : choice)}</p>
            <pre translate="no" className="whitespace-pre-wrap break-all font-mono text-xs text-secondary">{JSON.stringify([harness.command, ...(harness.args || [])], null, 2)}</pre>
            {!custom && "adapter" in harness && harness.adapter && <p className="text-xs leading-5 text-tertiary">{t("fleet.commandPinned", { adapter: harness.adapter })}</p>}
        </section>}
        <p className="text-xs leading-5 text-tertiary">{t("fleet.commandSessionPreferences")}</p>
        <p className="text-xs leading-5 text-warning-primary">{t("fleet.commandExecutionWarning")}</p>
        <p className="text-xs leading-5 text-tertiary">{t("fleet.commandObserverWarning")}</p>
        {receipt && <section role="status" className="min-w-0 space-y-2 rounded-lg border border-secondary p-3">
            <p className="text-sm font-medium text-primary">{t(bound ? "fleet.commandBound" : saved ? "fleet.commandSaved" : "fleet.commandSaveUnconfirmed")}</p>
            <p className="break-all text-xs text-secondary">{receipt.id} · {receipt.harness} @ {receipt.node}</p>
            <p className="break-all font-mono text-xs text-tertiary">{t("fleet.commandRevision", { revision: receipt.revision })}</p>
            <p className="text-xs leading-5 text-warning-primary">{t("fleet.commandUnverified")}</p>
            {!bound && <p className="text-xs leading-5 text-tertiary">{t(unknown ? "fleet.commandUnknownHint" : "fleet.commandRetryHint")}</p>}
        </section>}
        {!secure && <div role="alert" className="text-sm text-error-primary">{t("fleet.commandError.secureContext")}</div>}
        {error && <div role="alert" className="break-words text-sm text-error-primary">{error}</div>}
        {!bound && <div className="flex flex-wrap justify-end gap-2">
            {receipt && <Button size="sm" color="secondary" isLoading={busy} onClick={() => void act(true)}>{t("fleet.commandInspect")}</Button>}
            {(!receipt || receipt.phase === "saved" || receipt.phase === "bind-rejected") && <Button size="sm" color="primary" isLoading={busy} isDisabled={receipt ? busy : !valid || !!recovery.error} onClick={() => void act()}>{t(receipt ? "fleet.commandRetryBind" : custom ? "fleet.commandSaveAndBind" : "fleet.registerAgent")}</Button>}
        </div>}
    </div>;
}

function StringRows({ values, onChange, disabled, kind }: { values: string[]; onChange: (values: string[]) => void; disabled: boolean; kind: "argument" | "environment" }) {
    const { t } = useI18n();
    return <div className="flex min-w-0 flex-col gap-2">
        <p className="text-xs font-medium text-secondary">{t(kind === "argument" ? "fleet.commandArguments" : "fleet.commandEnvironment")}</p>
        {values.map((value, index) => <div key={index} className="flex min-w-0 items-center gap-2">
            <div className="min-w-0 flex-1"><Input size="sm" aria-label={t(kind === "argument" ? "settingsEditor.argument" : "fleet.commandEnvEntry", { index: index + 1 })} name={`fleet-agent-${kind}-${index}`} autoComplete="off" spellCheck="false" value={value} isDisabled={disabled} onChange={value => onChange(values.map((item, i) => index === i ? value : item))} /></div>
            <IconButton size="sm" color="tertiary" icon={Trash01} isDisabled={disabled} label={t(kind === "argument" ? "settingsEditor.removeArgument" : "fleet.commandRemoveEnv", { index: index + 1 })} onClick={() => onChange(values.filter((_, i) => i !== index))} />
        </div>)}
        <Button size="sm" color="link-gray" className="self-start" isDisabled={disabled} iconLeading={Plus} onClick={() => onChange([...values, ""])}>{t(kind === "argument" ? "settingsEditor.addArgument" : "fleet.commandAddEnv")}</Button>
    </div>;
}
