import type { CSSProperties, ReactNode } from "react";

// Topology draws who answers to whom. One node sits at the centre — the
// machine coordinating the fleet, or the machine a project's home lives
// on — and the rest stand on a ring around it. The spoke carries the
// health of that relationship, so the link and its status are one mark
// instead of two: a fleet with nothing wrong reads as quiet grey, and
// colour appears only where someone has to look.
//
// The same picture serves the fleet page and a project's drawer, which is
// why nothing here knows about nodes or workspaces — callers translate
// their own world into Spoke and hand it over.

export type TopologyTone = "ok" | "warn" | "down" | "idle";

export interface Spoke {
    id: string;
    label: string;
    // sub is the one demoted line under the name: a role, a workspace kind.
    sub?: string;
    // note says why this is not ok, and is the chip's title.
    note?: string;
    tone: TopologyTone;
}

// The ring is an ellipse because the box is wider than it is tall; equal
// radii would leave the sides empty and crowd the top and bottom.
const ringX = 32;
const ringY = 33;

const spokeLine: Record<TopologyTone, { cls: string; dash?: string; width: number }> = {
    ok: { cls: "text-fg-quaternary", width: 1 },
    warn: { cls: "text-fg-warning-primary", dash: "5 3", width: 1 },
    down: { cls: "text-fg-error-primary", dash: "1 4", width: 1 },
    idle: { cls: "text-fg-quaternary opacity-50", dash: "2 4", width: 1 },
};

const dot: Record<TopologyTone, string> = {
    ok: "bg-fg-success-primary",
    warn: "bg-fg-warning-primary",
    down: "bg-fg-error-primary",
    idle: "bg-fg-quaternary",
};

// Above this many spokes the ring stops being readable and the picture
// becomes a worse table than a table. The caller's list takes over.
export const topologyLimit = 9;

function Chip({ spoke, center, style }: { spoke: Spoke; center?: boolean; style?: CSSProperties }) {
    return (
        <div style={style}
            title={[spoke.label, spoke.sub, spoke.note].filter(Boolean).join(" · ")}
            className={`absolute flex max-w-28 min-w-0 sm:max-w-36 -translate-x-1/2 -translate-y-1/2 items-center gap-1.5 rounded-lg px-2 py-1.5 ${center ? "bg-brand-primary_alt ring-1 ring-brand" : "bg-secondary ring-1 ring-secondary"}`}>
            <span aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${dot[spoke.tone]}`} />
            <span className="flex min-w-0 flex-col">
                <span className={`truncate text-xs leading-4 font-medium ${center ? "text-brand-secondary" : "text-primary"}`}>{spoke.label}</span>
                {spoke.sub && <span className="truncate text-[11px] leading-4 text-tertiary">{spoke.sub}</span>}
            </span>
        </div>
    );
}

export function Topology({ center, spokes, legend, label }: { center: Spoke; spokes: Spoke[]; legend?: ReactNode; label: string }) {
    // One node has no relationships to draw; a crowd has too many.
    if (!spokes.length || spokes.length > topologyLimit) return null;
    // Start at the top and go clockwise, so the first machine in the
    // caller's order lands where the eye starts.
    const placed = spokes.map((spoke, i) => {
        const angle = -Math.PI / 2 + (i * 2 * Math.PI) / spokes.length;
        return { spoke, x: 50 + ringX * Math.cos(angle), y: 50 + ringY * Math.sin(angle) };
    });
    return (
        <figure role="group" aria-label={label} className="min-w-0 py-1">
            <div className="relative mx-auto h-48 w-full max-w-lg overflow-hidden sm:h-56">
                <svg aria-hidden="true" viewBox="0 0 100 100" preserveAspectRatio="none" className="absolute inset-0 size-full">
                    {placed.map(({ spoke, x, y }) => {
                        const line = spokeLine[spoke.tone];
                        return <line key={spoke.id} x1={50} y1={50} x2={x} y2={y} stroke="currentColor" strokeWidth={line.width}
                            strokeDasharray={line.dash} strokeLinecap="round" vectorEffect="non-scaling-stroke" className={line.cls} />;
                    })}
                </svg>
                <Chip spoke={center} center style={{ left: "50%", top: "50%" }} />
                {placed.map(({ spoke, x, y }) => <Chip key={spoke.id} spoke={spoke} style={{ left: `${x}%`, top: `${y}%` }} />)}
            </div>
            {legend && <figcaption className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-tertiary">{legend}</figcaption>}
        </figure>
    );
}

// LegendMark is one entry of the caption: the dot as the chips draw it,
// next to the words for what that dot means.
export function LegendMark({ tone, children }: { tone: TopologyTone; children: ReactNode }) {
    return <span className="inline-flex items-center gap-1.5"><span aria-hidden="true" className={`size-1.5 rounded-full ${dot[tone]}`} />{children}</span>;
}
