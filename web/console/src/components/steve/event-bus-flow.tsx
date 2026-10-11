import { useEffect, useRef, useState } from "react";
import { Dataflow04, Database02, PauseCircle, PlayCircle, Server02 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { AnimatedBeam } from "@/components/magicui/animated-beam";
import { Panel } from "@/components/steve/page";
import { AssistantFeedback } from "@/components/steve/assistant-projection";
import { EventBusOverview } from "@/components/steve/event-bus-overview";
import { useEventFlow, type FlowParticle } from "@/hooks/use-event-flow";
import { useI18n } from "@/providers/locale-provider";
import { number } from "@/lib/format";
import "./event-bus-flow.css";
import type { ServiceBacklog } from "@/lib/assistant-types";

const CELLS = 128;
const PIXELS = Array.from({ length: CELLS }, (_, index) => index);
function usePixelCount(target: number | undefined, motion: boolean) {
    const [count, setCount] = useState(0);
    const current = useRef<number | null>(null);
    useEffect(() => {
        if (target === undefined) { current.current = null; setCount(0); return; }
        const next = Math.min(target, CELLS);
        if (current.current === null || !motion) { current.current = next; setCount(next); return; }
        const start = current.current;
        const steps = Math.abs(next - start);
        const timers = Array.from({ length: steps }, (_, index) => window.setTimeout(() => {
            current.current = start + Math.sign(next - start) * (index + 1);
            setCount(current.current);
        }, 1800 * (index + 1) / steps));
        return () => timers.forEach(window.clearTimeout);
    }, [target, motion]);
    return count;
}
function FlowBoard({ queue, particles, finish, motion }: { queue: Record<string, number>; particles: FlowParticle[]; finish: (id: string) => void; motion: boolean }) {
    const { t, locale } = useI18n();
    const container = useRef<HTMLDivElement>(null);
    const publisher = useRef<HTMLDivElement>(null);
    const entry = useRef<HTMLSpanElement>(null);
    const exit = useRef<HTMLSpanElement>(null);
    const consumer = useRef<HTMLDivElement>(null);
    const incoming = particles.filter((item) => item.direction === "in");
    const outgoing = particles.filter((item) => item.direction === "out");
    const pixelCount = usePixelCount(queue.pending, motion);
    return <div className="bus-flow" data-bus-flow="ready" data-motion={motion ? "running" : "paused"}>
        <div className="bus-flow-labels"><span>{t("assistant.busFlowPublish")}</span><span>{t("assistant.busFlowQueue")}</span><span>{t("assistant.busFlowTake")}</span></div>
        <div className="bus-flow-track" ref={container}>
            <AnimatedBeam containerRef={container} fromRef={publisher} toRef={entry} curvature={30} gradientStartColor="var(--color-orange-400)" gradientStopColor="var(--color-purple-400)" pulses={incoming} onComplete={finish} />
            <AnimatedBeam containerRef={container} fromRef={exit} toRef={consumer} curvature={-30} gradientStartColor="var(--color-purple-400)" gradientStopColor="var(--color-brand-400)" pulses={outgoing} onComplete={finish} />
            <div className="bus-flow-port" ref={publisher} aria-hidden="true" data-active={incoming.length > 0}><Dataflow04 /></div>
            <div className="bus-flow-rail" data-flow-entered={incoming.length} />
            <div className="bus-flow-buffer">
                <span className="bus-flow-anchor bus-flow-anchor-in" ref={entry} />
                <span className="bus-flow-anchor bus-flow-anchor-out" ref={exit} />
                <div className="bus-flow-buffer-label"><Database02 aria-hidden="true" />{t("assistant.deliveryPending")}</div>
                <div className="bus-flow-values"><strong>{number(queue.pending, locale)}</strong></div>
                <div className="bus-flow-pixels" aria-hidden="true">{PIXELS.map((index) => <span key={index} style={{ gridRow: 8 - Math.floor(index / 16), gridColumn: 16 - index % 16 }} className={index < pixelCount ? "bus-flow-cell bus-flow-cell-filled" : "bus-flow-cell"} data-filled={index < pixelCount} />)}</div>
                {queue.pending > CELLS ? <span className="bus-flow-overflow">+{number(queue.pending - CELLS, locale)}</span> : null}
            </div>
            <div className="bus-flow-rail" data-flow-taken={outgoing.length} />
            <div className="bus-flow-port" ref={consumer} aria-hidden="true" data-active={outgoing.length > 0}><Server02 /></div>
        </div>
        <div className="bus-flow-labels bus-flow-counts"><span>{t("assistant.busFlowRouting", { count: number(queue.routing, locale) })}</span><span /><span>{t("assistant.busFlowActive", { count: number(queue.claimed + queue.accepted, locale) })}</span></div>
        <div className="bus-flow-bottom">
            <p className="bus-flow-legend"><span className="bus-flow-key" aria-hidden="true" />{t("assistant.busFlowPixel")}</p>
            <p className="bus-flow-hold"><span className="bus-flow-hold-pixels" aria-hidden="true">{Array.from({ length: Math.min(queue.unknown, 12) }, (_, index) => <i key={index} />)}</span><span>{t("assistant.busFlowHeld", { count: number(queue.unknown, locale) })}</span></p>
        </div>
        <p className="bus-flow-note">{t("assistant.busFlowSampling")}</p>
    </div>;
}

export function EventBusFlow({ overview, refreshOverview }: { overview: { value: ServiceBacklog | null; error: string; updated: string }; refreshOverview: () => void }) {
    const { t } = useI18n();
    const [paused, setPaused] = useState(false);
    const [reduced, setReduced] = useState(() => window.matchMedia("(prefers-reduced-motion: reduce)").matches);
    useEffect(() => {
        const media = window.matchMedia("(prefers-reduced-motion: reduce)");
        const update = () => setReduced(media.matches);
        media.addEventListener("change", update);
        return () => media.removeEventListener("change", update);
    }, []);
    const { snapshot, error, particles, finish, refresh } = useEventFlow(!paused && !reduced);
    const queue = snapshot && Object.fromEntries(Object.entries(snapshot.queues).map(([key, value]) => [key, value.pending]));
    return <><Panel title={t("assistant.busFlowTitle")} description={t("assistant.busFlowDescription")}
        aside={<Button size="sm" color="secondary" iconLeading={paused ? PlayCircle : PauseCircle} onClick={() => setPaused((value) => !value)} isDisabled={reduced}>{t(reduced ? "assistant.busFlowReduced" : paused ? "assistant.busFlowResume" : "assistant.busFlowPause")}</Button>}>
        <AssistantFeedback error={error} loading={!snapshot && !error} onRefresh={() => { void refresh(); }} />
        {queue ? <FlowBoard queue={queue} particles={particles} finish={finish} motion={!paused && !reduced} /> : null}
    </Panel><EventBusOverview {...overview} flow={snapshot} refresh={() => { void refresh(); refreshOverview(); }} /></>;
}
