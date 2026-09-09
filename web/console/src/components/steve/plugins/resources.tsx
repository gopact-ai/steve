import { Link } from "react-router";
import { Badge } from "@/components/base/badges/badges";
import { Panel } from "@/components/steve/page";
import type { PluginResource } from "@/lib/plugin-types";
import { useI18n } from "@/providers/locale-provider";

export function PluginResources({ resources }: { resources: PluginResource[] }) {
    const { t } = useI18n();
    if (!resources.length) return null;
    return <Panel title={t("plugins.managedResources")} description={t("plugins.managedHint")}>
        <ul className="flex min-w-0 flex-col divide-y divide-secondary">
            {resources.map((resource) => <li key={`${resource.installation}/${resource.name}`} className="flex min-w-0 flex-wrap items-center justify-between gap-2 py-3">
                <Link className="min-w-0 break-words text-sm font-medium text-brand-secondary outline-offset-4 focus-visible:outline-2" to={`/plugins?edit=${encodeURIComponent(resource.installation)}`}>{resource.package_id} · {resource.name} <span className="font-normal text-tertiary">{resource.version}</span></Link>
                <Badge type="pill-color" size="sm" color={resource.enabled ? "success" : "gray"}>{t(resource.enabled ? "plugins.enabled" : "plugins.disabled")}</Badge>
                <p className="w-full break-words text-xs text-tertiary">{t("plugins.projects")}: {resource.projects.join(", ")}</p>
            </li>)}
        </ul>
    </Panel>;
}
