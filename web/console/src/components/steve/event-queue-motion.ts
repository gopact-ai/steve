import type { FlowParticle } from "@/hooks/use-event-flow";

const INGRESS = 500, SHIFT = 260, STEP = 70, FADE = 100, WAIT_LIMIT = 8000;
const MIN_PITCH = 1.5, MIN_COLUMNS = 32, MAX_COLUMNS = 1024, CORE = 3;
const smooth = (value: number) => { const c = Math.max(0, Math.min(1, value)); return c * c * (3 - 2 * c); };

interface Scheduled extends FlowParticle { at: number }
interface Packet { id: string; direction: FlowParticle["direction"]; started: number; from: number; duration: number; x: number; opacity: number }
type Palette = Record<"idle" | "wait" | "in" | "out", string>;
// Diagnostics and acceptance surface: what the canvas shows, in track pixels.
export interface FlowState {
    length: number; inlet: number; span: number; slots: number; units: number; width: number; height: number; fill: number; lit: number[];
    packets: { id: string; direction: FlowParticle["direction"]; left: number; width: number; height: number; opacity: number }[];
}

// React owns observed facts. This renderer owns one canvas: a constant DOM,
// no per-light style writes, and no frame loop while the bus is quiet.
export function createEventQueueMotion(stage: HTMLDivElement, motion: boolean, finish: (id: string) => void, scale: (units: number) => void) {
    const canvas = stage.querySelector("canvas")!, buffer = stage.querySelector<HTMLDivElement>(".bus-flow-buffer")!;
    const context = canvas.getContext("2d")!;
    const scheduled: Scheduled[] = [], packets: Packet[] = [], shifts: number[] = [];
    const palette: Palette = { idle: "", wait: "", in: "", out: "" };
    let length = 0, height = 0, inlet = 0, span = 0, pitch = 0, width = 0;
    let slots = MIN_COLUMNS, units = 1, shrinkSince = 0, count = 0, target = 0, frame = 0;
    let seen = new Set<string>(), lastExit: Packet | null = null, destroyed = false, initialized = false;

    // Canvas cannot resolve var() or light-dark(); a styled element can.
    const readPalette = () => {
        for (const key of Object.keys(palette) as (keyof Palette)[]) {
            canvas.style.color = `var(--bus-${key})`;
            palette[key] = getComputedStyle(canvas).color;
        }
        canvas.style.color = "";
    };
    // Independent deadlines keep repeated claims from sliding the pile out.
    let memoAt = NaN, memo = 0;
    const offset = (now: number) => {
        if (memoAt !== now) { memoAt = now; memo = shifts.reduce((sum, started) => sum + 1 - smooth((now - started) / SHIFT), 0) / units; }
        return memo;
    };
    const slotX = (slot: number, now: number) => inlet + span - width - (slot + offset(now)) * pitch;
    const configure = (demand: number, now: number, immediate = false) => {
        if (!span) return;
        // Grow for the backlog with room to receive; group only below a readable light.
        const maximum = Math.max(MIN_COLUMNS, Math.min(MAX_COLUMNS, Math.floor(span / MIN_PITCH)));
        const reserve = Math.max(8, Math.ceil(demand * .25));
        const nextUnits = demand + 4 <= maximum ? 1 : Math.ceil((demand + reserve) / maximum);
        const nextSlots = Math.max(MIN_COLUMNS, Math.min(maximum, Math.ceil((demand + reserve) / nextUnits / 16) * 16));
        const crowded = demand + 4 > slots * units, smaller = demand < slots * units * .35;
        shrinkSince = smaller ? shrinkSince || now : 0;
        // Hysteresis prevents zooming on every producer/consumer read.
        if (!immediate && !crowded && !(smaller && now - shrinkSince >= 8000)) return;
        if (nextUnits !== units) scale(nextUnits);
        slots = nextSlots; units = nextUnits; memoAt = NaN;
        pitch = span / slots; width = pitch - Math.min(2, pitch * .22);
    };
    const reset = () => {
        const active = [...scheduled, ...packets].map((item) => item.id);
        scheduled.length = 0; packets.length = 0; shifts.length = 0; lastExit = null; memoAt = NaN;
        active.forEach(finish);
        count = target;
        configure(target, performance.now(), true);
        paint(performance.now());
    };
    const measure = () => {
        const track = stage.getBoundingClientRect(), queue = buffer.getBoundingClientRect();
        if (track.width <= 0 || queue.width <= 0) return;
        // ResizeObserver also reports on mount; an unchanged space keeps activity.
        if (length === track.width && span === queue.width && inlet === queue.left - track.left) return;
        length = track.width; height = track.height; span = queue.width; inlet = queue.left - track.left;
        const ratio = window.devicePixelRatio || 1;
        canvas.width = Math.round(length * ratio); canvas.height = Math.round(height * ratio);
        context.setTransform(ratio, 0, 0, ratio, 0, 0);
        readPalette();
        reset(); // A new coordinate space establishes a new baseline.
    };
    const spawn = (item: Scheduled, now: number) => {
        const outgoing = item.direction === "out", from = outgoing ? slotX(0, now) : -width;
        // Arrivals cruise to the tail; departures share one rail speed so none catch up.
        const cruise = Math.max(pitch / STEP, span / 2200), rail = span / MIN_COLUMNS / STEP;
        const duration = outgoing ? (length - from) / rail : INGRESS + Math.max(0, slotX(count / units, now) - inlet) / cruise;
        const packet: Packet = { id: item.id, direction: item.direction, started: now, from, duration, x: from, opacity: 0 };
        packets.push(packet);
        if (outgoing) { count--; shifts.push(now); lastExit = packet; memoAt = NaN; }
    };
    const advance = (now: number) => {
        for (let i = 0; i < scheduled.length;) {
            const item = scheduled[i];
            // A claim can share a read with its publication: wait for a real head that
            // has reached the outlet, and for the previous departure to clear one wide light.
            const spaced = offset(now) * units < .35 && (!lastExit || !packets.includes(lastExit) || lastExit.x - slotX(0, now) >= span / MIN_COLUMNS);
            const blocked = item.direction === "out" && (count < 1 || !spaced);
            if (item.at > now || (blocked && now - item.at < WAIT_LIMIT)) { i++; continue; }
            scheduled.splice(i, 1);
            if (blocked) finish(item.id); else spawn(item, now);
        }
        let lane = 0;
        for (let i = 0; i < packets.length;) {
            const packet = packets[i], age = now - packet.started, progress = Math.min(1, age / packet.duration);
            if (packet.direction === "in") {
                // Each arrival owns the next tail position, so concurrent intake never stacks.
                const tail = slotX((count + lane++) / units, now);
                packet.x = packet.from + (tail - packet.from) * (1 - (1 - progress) ** 3);
                packet.opacity = Math.min(1, progress * packet.duration / (INGRESS * .16));
            } else {
                packet.x = packet.from + (length - packet.from) * progress;
                packet.opacity = Math.max(0, Math.min(1, (packet.duration - age) / FADE));
            }
            if (progress < 1) { i++; continue; }
            if (packet.direction === "in") { count++; lane--; }
            packets.splice(i, 1); finish(packet.id);
        }
        for (let i = shifts.length - 1; i >= 0; i--) if (now - shifts[i] >= SHIFT) { shifts.splice(i, 1); memoAt = NaN; }
        // Bounded activity is not an authoritative delivery count.
        if (!packets.length && !scheduled.length) count = target;
    };
    // Waiting lights rest right-aligned from the head; a partial light is a partial group.
    const pile = (now: number) => {
        const parked = count / units, lights: [number, number][] = [];
        for (let i = 0; i < Math.ceil(parked); i++) {
            const fill = Math.min(1, parked - i), x = slotX(i, now) + width * (1 - fill);
            if (x + width * fill > inlet && x < inlet + span) lights.push([x, width * fill]);
        }
        return lights;
    };
    const bars = (color: string, alpha: number, items: [number, number][]) => {
        const y = (height - CORE) / 2;
        context.fillStyle = color;
        for (const [glow, grow] of [[.22, 3], [1, 0]]) {
            context.globalAlpha = alpha * glow; context.beginPath();
            for (const [x, w] of items) context.rect(x - grow / 2, y - grow, w + grow, CORE + grow * 2);
            context.fill();
        }
    };
    function paint(now: number) {
        if (!span) return;
        context.clearRect(0, 0, length, height);
        context.globalAlpha = 1; context.fillStyle = palette.idle; context.beginPath();
        for (let i = 0; i < slots; i++) context.rect(inlet + span - width - i * pitch, (height - CORE) / 2, width, CORE);
        context.fill();
        context.save(); context.beginPath(); context.rect(inlet, 0, span, height); context.clip();
        bars(palette.wait, 1, pile(now));
        context.restore();
        for (const packet of packets) bars(packet.direction === "in" ? palette.in : palette.out, packet.opacity, [[packet.x, width]]);
        context.globalAlpha = 1;
    }
    const tick = (now: number) => {
        frame = 0;
        if (destroyed || document.hidden) return;
        advance(now); paint(now);
        if (scheduled.length || packets.length || shifts.length) frame = requestAnimationFrame(tick);
    };
    const wake = () => { if (motion && span && !document.hidden && !frame) frame = requestAnimationFrame(tick); };
    const visibility = () => { cancelAnimationFrame(frame); frame = 0; reset(); };
    const flowState = (): FlowState => ({
        length, inlet, span, slots, units, width, height: CORE, fill: count / units, lit: pile(performance.now()).map(([x]) => x),
        packets: packets.map((item) => ({ id: item.id, direction: item.direction, left: item.x, width, height: CORE, opacity: item.opacity })),
    });
    Object.assign(stage, { flowState });
    const observer = new ResizeObserver(measure);
    observer.observe(stage);
    document.addEventListener("visibilitychange", visibility);
    measure();
    return {
        sync(pending: number, particles: FlowParticle[]) {
            target = pending;
            readPalette();
            if (!motion || document.hidden || !span) { reset(); particles.forEach((item) => finish(item.id)); return; }
            const now = performance.now(), retained = new Set(particles.map((item) => item.id));
            // Bound renderer activity along with the hook's 192 marks.
            for (let i = scheduled.length - 1; i >= 0; i--) if (!retained.has(scheduled[i].id)) scheduled.splice(i, 1);
            for (let i = packets.length - 1; i >= 0; i--) if (!retained.has(packets[i].id)) packets.splice(i, 1);
            for (const item of particles) if (!seen.has(item.id)) scheduled.push({ ...item, at: now + item.delay });
            seen = retained;
            const arrivals = scheduled.filter((item) => item.direction === "in").length + packets.filter((item) => item.direction === "in").length;
            const departures = scheduled.filter((item) => item.direction === "out").length;
            count = Math.max(0, pending - arrivals + departures);
            configure(Math.max(pending, count), now, !initialized); initialized = true;
            paint(now); wake();
        },
        destroy() {
            destroyed = true; cancelAnimationFrame(frame); observer.disconnect();
            document.removeEventListener("visibilitychange", visibility);
            delete (stage as Partial<{ flowState: unknown }>).flowState;
        },
    };
}
