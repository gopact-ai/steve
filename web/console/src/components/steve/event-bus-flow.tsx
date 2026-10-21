import { useEffect, useRef, useState, type ReactNode } from "react";
import { RefreshCw01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { createEventQueueMotion } from "@/components/steve/event-queue-motion";
import { Panel } from "@/components/steve/page";
import { AssistantFeedback } from "@/components/steve/assistant-projection";
import { EventBusOverview } from "@/components/steve/event-bus-overview";
import { useEventFlow, type FlowParticle } from "@/hooks/use-event-flow";
import { useI18n } from "@/providers/locale-provider";
import { dateTime, number } from "@/lib/format";
import "./event-bus-flow.css";
import type { ServiceBacklog } from "@/lib/assistant-types";

function FlowBoard({ pending, particles, finish, motion }: { pending: number; particles: FlowParticle[]; finish: (id: string) => void; motion: boolean }) {
    const { t, locale } = useI18n();
    const [scale, setScale] = useState(1);
    const stage = useRef<HTMLDivElement>(null);
    const renderer = useRef<ReturnType<typeof createEventQueueMotion> | null>(null);
    useEffect(() => {
        if (!stage.current) return;
        const current = createEventQueueMotion(stage.current, motion, finish, setScale);
        renderer.current = current;
        return () => { current.destroy(); renderer.current = null; };
    }, [motion, finish]);
    useEffect(() => { renderer.current?.sync(pending, particles); }, [pending, particles, motion]);
    return <div className="bus-flow" data-bus-flow="ready" data-motion={motion ? "running" : "static"}>
        <div className="bus-flow-labels"><span>{t("assistant.busFlowPublish")}</span><span className="bus-flow-values">{t("assistant.busFlowQueue")} <strong>{number(pending, locale)}</strong>{scale > 1 ? <span className="bus-flow-scale" role="img" title={t("assistant.busFlowScale", { count: number(scale, locale) })} aria-label={t("assistant.busFlowScale", { count: number(scale, locale) })}>×{number(scale, locale)}</span> : null}</span><span>{t("assistant.busFlowTake")}</span></div>
        <div className="bus-flow-track" ref={stage} aria-hidden="true">
            <div className="bus-flow-rail bus-flow-rail-in" data-flow-entered={particles.filter((item) => item.direction === "in").length} />
            <div className="bus-flow-buffer" />
            <canvas className="bus-flow-canvas" />
            <div className="bus-flow-rail bus-flow-rail-out" data-flow-taken={particles.filter((item) => item.direction === "out").length} />
        </div>
    </div>;
}

export function EventBusFlow({ overview, refreshOverview, providers }: { overview: { value: ServiceBacklog | null; error: string; updated: string }; refreshOverview: () => void; providers: ReactNode }) {
    const { t, locale } = useI18n();
    const [reduced, setReduced] = useState(() => window.matchMedia("(prefers-reduced-motion: reduce)").matches);
    useEffect(() => {
        const media = window.matchMedia("(prefers-reduced-motion: reduce)");
        const update = () => setReduced(media.matches);
        media.addEventListener("change", update);
        return () => media.removeEventListener("change", update);
    }, []);
    const { snapshot, error, particles, finish, refresh } = useEventFlow(!reduced);
    const reload = () => { void refresh(); refreshOverview(); };
    return <>
        <EventBusOverview {...overview} flow={snapshot} refresh={reload} />
        <div className="bus-dashboard-live">
            <Panel title={t("assistant.busOverview")} padding="flush"
                badge={snapshot ? <Badge type="pill-color" size="sm" color="success"><span className="bus-live-dot" aria-hidden="true" />{t("assistant.busLive")}</Badge> : null}
                aside={<Button size="sm" color="tertiary" iconLeading={RefreshCw01} onClick={reload} aria-label={t("assistant.refresh")} />}>
                <div className="bus-flow-content">
                    <AssistantFeedback error={error} loading={!snapshot && !error} onRefresh={() => { void refresh(); }} />
                    {snapshot ? <FlowBoard key={snapshot.capacity} pending={snapshot.queues.pending.pending} particles={particles} finish={finish} motion={!reduced} /> : null}
                </div>
                {snapshot ? <div className="bus-flow-meta"><span>{t("assistant.busFlowCadence")}</span><time dateTime={snapshot.observed_at}>{dateTime(snapshot.observed_at, locale, { hour: "2-digit", minute: "2-digit", second: "2-digit" })}</time></div> : null}
            </Panel>
            {providers}
        </div>
    </>;
}
