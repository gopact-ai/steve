import { useEffect, type ReactNode } from "react";
import { X } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Sheet } from "@/components/steve/drawer";
import { useI18n } from "@/providers/locale-provider";
import type { PluginManifest } from "@/lib/plugin-types";

export function PluginSheet({ title, dirty = false, busy = false, onClose, children }: { title: string; dirty?: boolean; busy?: boolean; onClose: () => void; children: ReactNode }) {
    const { t } = useI18n();
    useEffect(() => {
        if (!dirty && !busy) return;
        const before = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
        window.addEventListener("beforeunload", before);
        return () => window.removeEventListener("beforeunload", before);
    }, [dirty, busy]);
    const close = () => { if (!busy && (!dirty || window.confirm(t("plugins.unsaved")))) onClose(); };
    return <Sheet label={title} width={640} onClose={close}>
        <div className="workbench-drawer-header"><h2 className="min-w-0 flex-1 text-base font-semibold text-primary">{title}</h2><Button size="sm" color="tertiary" iconLeading={X} aria-label={t("plugins.close")} isDisabled={busy} onClick={close} /></div>
        <div className="workbench-drawer-body">{children}</div>
    </Sheet>;
}
export function PluginStatus({ state }: { state: string }) {
    const { t } = useI18n();
    const label = state === "prepared" ? t("plugins.prepared") : state === "offline" ? t("plugins.offline") : state === "unavailable" ? t("plugins.unavailable") : t("plugins.pending");
    return <Badge type="pill-color" size="sm" color={state === "prepared" ? "success" : state === "unavailable" ? "error" : state === "offline" ? "warning" : "gray"}>{label}</Badge>;
}
export function CapabilitySummary({ manifest }: { manifest: PluginManifest }) {
    const { t } = useI18n();
    return <div className="flex flex-wrap gap-2 text-xs text-tertiary">
        <span>{t("plugins.skills")} <span className="font-medium tabular-nums text-secondary">{Object.keys(manifest.skills ?? {}).length}</span></span>
        <span>· {t("plugins.tools")} <span className="font-medium tabular-nums text-secondary">{Object.keys(manifest.mcp ?? {}).length}</span></span>
        <span>· {t("plugins.presets")} <span className="font-medium tabular-nums text-secondary">{Object.keys(manifest.agents ?? {}).length}</span></span>
    </div>;
}
export const pluginError = (error: unknown) => error instanceof Error ? error.message : String(error);
export function sameDeclaration(left: unknown, right: unknown): boolean {
    const normalize = (value: unknown): unknown => Array.isArray(value) ? value.map(normalize) : value && typeof value === "object" ? Object.fromEntries(Object.entries(value).sort(([a], [b]) => a.localeCompare(b)).map(([key, item]) => [key, normalize(item)])) : value;
    return JSON.stringify(normalize(left)) === JSON.stringify(normalize(right));
}

export function ManifestDetails({ manifest }: { manifest: PluginManifest }) {
    const { t } = useI18n();
    return <details className="min-w-0 rounded-lg border border-secondary p-3">
        <summary className="cursor-pointer text-sm font-medium text-secondary focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring">{t("plugins.inspect")}</summary>
        <div className="mt-3 flex min-w-0 flex-col gap-3 text-xs">
            {Object.entries(manifest.skills ?? {}).map(([name, path]) => <p key={`skill/${name}`} className="break-all"><strong>{t("plugins.skills")} · {name}</strong> <span className="text-tertiary">{path}</span></p>)}
            {Object.entries(manifest.mcp ?? {}).map(([name, server]) => <div key={`mcp/${name}`} className="flex min-w-0 flex-col gap-1"><strong>{name} · {server.transport}</strong><code className="break-all text-tertiary">{server.program ? [server.program.runtime, server.program.path].filter(Boolean).join(" ") : server.url?.text ?? `${t("plugins.configuration")}: ${server.url?.config ?? ""}`}</code>{server.program?.args?.length ? <code className="break-all text-tertiary">{JSON.stringify(server.program.args)}</code> : null}</div>)}
            {Object.entries(manifest.settings ?? {}).map(([name, setting]) => <p key={`setting/${name}`} className="break-words"><strong>{name}{setting.secret ? ` · ${t("plugins.credentialReference")}` : ""}</strong> {setting.description}</p>)}
            {Object.entries(manifest.agents ?? {}).map(([name, agent]) => <p key={`agent/${name}`} className="break-words"><strong>{t("plugins.presets")} · {name} · {agent.harness}</strong><span className="mt-1 block whitespace-pre-wrap text-tertiary">{agent.system_prompt}</span></p>)}
        </div>
    </details>;
}
