import { useEffect, useRef, useState, type ReactNode } from "react";
import { AnimatePresence, motion as animate } from "motion/react";
import { RefreshCw01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { AnimatedBeam } from "@/components/magicui/animated-beam";
import { Panel } from "@/components/steve/page";
import { AssistantFeedback } from "@/components/steve/assistant-projection";
import { EventBusOverview } from "@/components/steve/event-bus-overview";
import { useEventFlow, type FlowParticle } from "@/hooks/use-event-flow";
import { useI18n } from "@/providers/locale-provider";
import { dateTime, number } from "@/lib/format";
import "./event-bus-flow.css";
import type { ServiceBacklog } from "@/lib/assistant-types";

function useQueueDots(target: number, capacity: number) {
    const [dots, setDots] = useState<number[]>([]);
    const current = useRef<number[] | null>(null);
    const sequence = useRef(0);
    useEffect(() => {
        const next = Math.min(target, capacity);
        const resize = (size: number) => {
            const before = current.current ?? [];
            // Oldest dot is on the right. Claims remove that head; new dots
            // join the left of the retained row. These are visual slots only.
            current.current = size < before.length ? before.slice(before.length - size) : [...before, ...Array.from({ length: size - before.length }, () => sequence.current++)];
            setDots(current.current);
        };
        // Counts are a current snapshot, not a progress simulation. Reconcile
        // immediately; Motion handles only the short visual position changes.
        resize(next);
    }, [target, capacity]);
    return dots;
}
function FlowBoard({ queue, particles, finish, motion }: { queue: Record<string, number>; particles: FlowParticle[]; finish: (id: string) => void; motion: boolean }) {
    const { t, locale } = useI18n();
    const container = useRef<HTMLDivElement>(null);
    const publisher = useRef<HTMLSpanElement>(null);
    const entry = useRef<HTMLSpanElement>(null);
    const exit = useRef<HTMLSpanElement>(null);
    const consumer = useRef<HTMLSpanElement>(null);
    const buffer = useRef<HTMLDivElement>(null);
    const [capacity, setCapacity] = useState(8);
    useEffect(() => {
        const observer = new ResizeObserver(([size]) => setCapacity(Math.min(128, Math.max(1, Math.floor((size.contentRect.width + 4) / 12)))));
        if (buffer.current) observer.observe(buffer.current);
        return () => observer.disconnect();
    }, []);
    const incoming = particles.filter((item) => item.direction === "in");
    const outgoing = particles.filter((item) => item.direction === "out");
    const dots = useQueueDots(queue.pending, capacity);
    const visible = Math.min(queue.pending, capacity);
    return <div className="bus-flow" data-bus-flow="ready" data-motion={motion ? "running" : "static"}>
        <div className="bus-flow-labels"><span>{t("assistant.busFlowPublish")}</span><span className="bus-flow-values">{t("assistant.busFlowQueue")} <strong>{number(queue.pending, locale)}</strong>{queue.pending > capacity ? <span className="bus-flow-overflow">+{number(queue.pending - capacity, locale)}</span> : null}</span><span>{t("assistant.busFlowTake")}</span></div>
        <div className="bus-flow-track" ref={container} aria-hidden="true">
            <AnimatedBeam containerRef={container} fromRef={publisher} toRef={entry} layoutKey={visible} packet="block" gradientStartColor="var(--color-orange-400)" gradientStopColor="var(--color-purple-400)" pulses={incoming} onComplete={finish} />
            <AnimatedBeam containerRef={container} fromRef={exit} toRef={consumer} packet="block" gradientStartColor="var(--color-purple-400)" gradientStopColor="var(--color-brand-400)" pulses={outgoing} onComplete={finish} />
            <span className="bus-flow-anchor bus-flow-start" ref={publisher} />
            <div className="bus-flow-rail bus-flow-rail-in" data-flow-entered={incoming.length} />
            <div className="bus-flow-buffer">
                <span className="bus-flow-anchor bus-flow-anchor-in" style={{ right: `min(calc(100% - 14px), ${visible * 12 + 14}px)` }} ref={entry} />
                <span className="bus-flow-anchor bus-flow-anchor-out" ref={exit} />
                <div className="bus-flow-dots" ref={buffer}><AnimatePresence initial={false} mode="popLayout">{dots.map((id) => <animate.span key={id} className="bus-flow-cell" data-filled="true" layout={motion ? "position" : false} initial={motion ? { opacity: 0, x: -12 } : false} animate={{ opacity: 1, x: 0 }} exit={motion ? { opacity: 0, x: 12 } : { opacity: 0 }} transition={{ duration: motion ? .18 : 0 }} />)}</AnimatePresence></div>
            </div>
            <div className="bus-flow-rail bus-flow-rail-out" data-flow-taken={outgoing.length} />
            <span className="bus-flow-anchor bus-flow-end" ref={consumer} />
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
    const queue = snapshot && Object.fromEntries(Object.entries(snapshot.queues).map(([key, value]) => [key, value.pending]));
    const reload = () => { void refresh(); refreshOverview(); };
    return <>
        <EventBusOverview {...overview} flow={snapshot} refresh={reload} />
        <div className="bus-dashboard-live">
            <Panel title={t("assistant.busOverview")} padding="flush"
                badge={snapshot ? <Badge type="pill-color" size="sm" color="success"><span className="bus-live-dot" aria-hidden="true" />{t("assistant.busLive")}</Badge> : null}
                aside={<Button size="sm" color="tertiary" iconLeading={RefreshCw01} onClick={reload} aria-label={t("assistant.refresh")} />}>
                <div className="bus-flow-content">
                    <AssistantFeedback error={error} loading={!snapshot && !error} onRefresh={() => { void refresh(); }} />
                    {queue ? <FlowBoard queue={queue} particles={particles} finish={finish} motion={!reduced} /> : null}
                </div>
                {snapshot ? <div className="bus-flow-meta"><span>{t("assistant.busFlowCadence")}</span><time dateTime={snapshot.observed_at}>{dateTime(snapshot.observed_at, locale, { hour: "2-digit", minute: "2-digit", second: "2-digit" })}</time></div> : null}
            </Panel>
            {providers}
        </div>
    </>;
}
