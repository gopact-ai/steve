import type { FlowParticle } from "@/hooks/use-event-flow";

const INGRESS = 500;
const HANDOFF = 120;
const CELL_STEP = 70;
const SHIFT = 260;
const smooth = (value: number) => {
    const clamped = Math.max(0, Math.min(1, value));
    return clamped * clamped * (3 - 2 * clamped);
};

interface Geometry { width: number; height: number; pitch: number; inlet: number; last: number; length: number }
interface Packet { el: HTMLSpanElement; animation: Animation; handoff: number | null }
interface Transit { id: string; position: number; entered: number; lastFrame: number; initialSpeed: number; bridge: Packet | null }
interface Scheduled extends FlowParticle { at: number }

// React owns the snapshot and the fixed slot nodes. This renderer owns only
// transient light levels/packets: no per-frame React commits or layout reads.
export function createEventQueueMotion(stage: HTMLDivElement, motion: boolean, finish: (id: string) => void) {
    const cells = Array.from(stage.querySelectorAll<HTMLSpanElement>(".bus-flow-cell"));
    const levels = new Float64Array(cells.length);
    const painted = new Float64Array(cells.length).fill(-1);
    const transits: Transit[] = [];
    const scheduled: Scheduled[] = [];
    const packets = new Set<Packet>();
    let seen = new Set<string>();
    let geometry: Geometry | null = null;
    let shift: { started: number; wait: number; amount: number } | null = null;
    let count = 0, target = 0, parkedBefore = -1, shiftBefore = -1;
    let frame = 0, destroyed = false, lastDeparture = -Infinity;

    const shiftAmount = (now: number) => {
        if (!shift) return 0;
        const amount = shift.amount * (1 - smooth((now - shift.started - shift.wait) / SHIFT));
        if (amount === 0) shift = null;
        return amount;
    };
    const measure = () => {
        const first = cells[0].getBoundingClientRect();
        const second = cells[1].getBoundingClientRect();
        const last = cells[cells.length - 1].getBoundingClientRect();
        const rect = stage.getBoundingClientRect();
        if (first.width <= 0 || rect.width <= 0) return;
        geometry = { width: first.width, height: first.height, pitch: second.left - first.left, inlet: first.left - rect.left, last: last.left - rect.left, length: rect.width };
        stage.style.setProperty("--bus-cell-width", `${first.width}px`);
        stage.style.setProperty("--bus-cell-height", `${first.height}px`);
        // A resize changes the coordinate space. Reconcile rather than teleport
        // existing packets between two differently sized tracks.
        reset();
    };
    const removePacket = (packet: Packet | null) => {
        if (!packet) return;
        packets.delete(packet);
        packet.animation.cancel();
        packet.el.remove();
    };
    const completeTransit = (index: number) => {
        const [transit] = transits.splice(index, 1);
        removePacket(transit.bridge);
        finish(transit.id);
    };
    const reset = () => {
        const active = new Set([...scheduled.map((item) => item.id), ...transits.map((item) => item.id), ...Array.from(packets, (packet) => packet.el.dataset.activity!)]);
        for (const packet of packets) removePacket(packet);
        active.forEach(finish);
        transits.length = 0;
        scheduled.length = 0;
        shift = null;
        count = target;
        paint(performance.now());
    };
    const spawn = (item: Scheduled, now: number) => {
        const g = geometry!;
        const outgoing = item.direction === "out";
        const el = document.createElement("span");
        el.className = `bus-flow-particle ${outgoing ? "is-out" : "is-in"}`;
        el.dataset.activity = item.id;
        stage.append(el);
        const from = outgoing ? g.last - shiftAmount(now) * g.pitch : -g.width;
        const to = outgoing ? g.length : g.inlet;
        let duration = INGRESS, clearance = 0;
        let keyframes: Keyframe[];
        if (outgoing) {
            const speed = (g.inlet + g.width) / INGRESS;
            const distance = Math.min((to - from) * .4, speed * Math.max(140, 2 * g.pitch / speed) / 2);
            const launch = 2 * distance / speed;
            clearance = g.pitch <= distance ? Math.sqrt(2 * g.pitch * launch / speed) : launch + (g.pitch - distance) / speed;
            duration = launch + (to - from - distance) / speed;
            const fade = Math.min(100, (duration - launch) * .25);
            keyframes = [
                { transform: `translateX(${from}px)`, opacity: 1, offset: 0, easing: "cubic-bezier(.333333,0,.666667,.333333)" },
                { transform: `translateX(${from + distance}px)`, opacity: 1, offset: launch / duration },
                { transform: `translateX(${to - speed * fade}px)`, opacity: 1, offset: 1 - fade / duration },
                { transform: `translateX(${to}px)`, opacity: 0, offset: 1 },
            ];
        } else {
            keyframes = [
                { transform: `translateX(${from}px)`, opacity: 0 },
                { transform: `translateX(${from + (to - from) * .16}px)`, opacity: 1, offset: .16 },
                { transform: `translateX(${to}px)`, opacity: 1 },
            ];
        }
        const packet: Packet = { el, animation: el.animate(keyframes, { duration, easing: "linear", fill: "both" }), handoff: null };
        packets.add(packet);
        if (outgoing) {
            const offset = shiftAmount(now);
            shift = { started: now, wait: clearance, amount: offset + 1 };
            count--;
            lastDeparture = now;
            packet.animation.finished.then(() => {
                if (destroyed || !packets.has(packet)) return;
                removePacket(packet);
                finish(item.id);
            }).catch(() => {});
        } else {
            packet.animation.finished.then(() => {
                if (destroyed || !packets.has(packet)) return;
                packet.handoff = performance.now();
                count++;
            }).catch(() => {});
            transits.push({ id: item.id, position: 0, entered: now, lastFrame: now, initialSpeed: (g.inlet + g.width) / INGRESS / g.pitch, bridge: packet });
        }
    };
    const distanceAt = (transit: Transit, now: number) => {
        const age = Math.max(0, now - transit.entered), u = Math.min(1, age / HANDOFF);
        const initial = transit.initialSpeed, cruise = 1 / CELL_STEP;
        return age >= HANDOFF ? (initial + cruise) * HANDOFF / 2 + (age - HANDOFF) * cruise
            : initial * age + (cruise - initial) * HANDOFF * (u ** 3 - u ** 4 / 2);
    };
    const admitted = () => {
        let total = 0;
        for (const item of transits) if (!item.bridge || item.bridge.handoff !== null) total++;
        return total;
    };
    const parkedCount = () => Math.min(cells.length, Math.max(0, count - admitted()));
    const advance = (now: number) => {
        for (let i = 0; i < scheduled.length;) {
            const item = scheduled[i];
            // Claims can arrive in the same read as their publication. Wait for
            // a visible head rather than emit a disconnected copy on the right.
            if (item.at > now || (item.direction === "out" && (parkedCount() === 0 || now - lastDeparture < 120))) { i++; continue; }
            scheduled.splice(i, 1);
            spawn(item, now);
        }
        for (let i = 0; i < transits.length;) {
            const transit = transits[i];
            if (transit.bridge) {
                if (transit.bridge.handoff === null) { i++; continue; }
                if (transit.entered < transit.bridge.handoff) {
                    transit.entered = transit.bridge.handoff;
                    transit.lastFrame = transit.entered;
                    transit.bridge.el.style.transform = `translateX(${geometry!.inlet}px)`;
                    transit.bridge.el.style.opacity = "1";
                    transit.bridge.animation.cancel();
                }
            }
            const shifting = shiftAmount(now);
            const frontier = cells.length - parkedCount() - 1 - shifting;
            const limit = i === 0 ? frontier : Math.min(frontier, transits[i - 1].position - 1);
            const distance = distanceAt(transit, now) - distanceAt(transit, transit.lastFrame);
            transit.position = Math.max(transit.position, Math.min(Math.max(0, limit), transit.position + distance));
            transit.lastFrame = now;
            const joined = now - transit.entered >= HANDOFF;
            if (i === 0 && joined && (frontier < 0 || (transit.position >= frontier && shifting === 0))) { completeTransit(i); continue; }
            i++;
        }
        // Activity history is bounded and publications need not create exactly
        // one delivery. Never infer the durable backlog from replayed lights.
        if (!transits.length && !scheduled.length) count = target;
    };
    function paint(now: number) {
        const parked = parkedCount();
        levels.fill(0);
        levels.fill(1, cells.length - parked);
        for (const transit of transits) {
            const cell = Math.floor(transit.position), fraction = transit.position - cell;
            const appearance = transit.bridge ? (transit.bridge.handoff === null ? 0 : smooth((now - transit.entered) / HANDOFF)) : 1;
            levels[cell] = Math.min(1, levels[cell] + (1 - fraction) * appearance);
            if (cell + 1 < cells.length) levels[cell + 1] = Math.min(1, levels[cell + 1] + fraction * appearance);
            if (transit.bridge && transit.bridge.handoff !== null) {
                transit.bridge.el.style.transform = `translateX(${geometry!.inlet + transit.position * geometry!.pitch}px)`;
                transit.bridge.el.style.opacity = String(1 - appearance);
                transit.bridge.el.style.setProperty("--bus-packet-color", colorAt(transit.position));
                if (appearance === 1) { removePacket(transit.bridge); transit.bridge = null; }
            }
        }
        const offset = Math.round(shiftAmount(now) * 10000) / 10000;
        if (offset !== shiftBefore) {
            stage.style.setProperty("--bus-shift-offset", `${-offset * (geometry?.pitch ?? 0)}px`);
            // Interpolate the color at the light's physical position as it shifts.
            stage.style.setProperty("--bus-shift-tint", `${Math.min(1, offset) * 100}%`);
            shiftBefore = offset;
        }
        for (let i = 0; i < cells.length; i++) {
            const filled = i >= cells.length - parked;
            if (parked !== parkedBefore) cells[i].dataset.filled = String(filled);
            const level = Math.round(levels[i] * 1000) / 1000;
            if (level !== painted[i]) { cells[i].style.setProperty("--bus-level", String(level)); painted[i] = level; }
        }
        parkedBefore = parked;
    }
    const tick = (now: number) => {
        frame = 0;
        if (destroyed || document.hidden) return;
        advance(now);
        paint(now);
        if (scheduled.length || transits.length || shift) frame = requestAnimationFrame(tick);
    };
    const wake = () => { if (motion && geometry && !document.hidden && !frame) frame = requestAnimationFrame(tick); };
    const visibility = () => {
        cancelAnimationFrame(frame); frame = 0;
        reset();
    };
    const observer = new ResizeObserver(measure);
    observer.observe(stage);
    document.addEventListener("visibilitychange", visibility);
    measure();
    return {
        sync(pending: number, particles: FlowParticle[]) {
            target = pending;
            if (!motion || document.hidden || !geometry) { reset(); particles.forEach((item) => finish(item.id)); return; }
            const now = performance.now();
            const retained = new Set(particles.map((item) => item.id));
            // The hook bounds activity to 192 marks. Keep the renderer bounded
            // too when an overloaded feed drops marks faster than we play them.
            for (let i = scheduled.length - 1; i >= 0; i--) if (!retained.has(scheduled[i].id)) scheduled.splice(i, 1);
            for (let i = transits.length - 1; i >= 0; i--) if (!retained.has(transits[i].id)) {
                removePacket(transits[i].bridge); transits.splice(i, 1);
            }
            for (const packet of packets) if (!retained.has(packet.el.dataset.activity!)) removePacket(packet);
            for (const item of particles) {
                if (seen.has(item.id)) continue;
                seen.add(item.id);
                scheduled.push({ ...item, at: now + item.delay });
            }
            seen = retained;
            const arrivals = scheduled.filter((item) => item.direction === "in").length + transits.filter((item) => item.bridge?.handoff === null).length;
            const departures = scheduled.filter((item) => item.direction === "out").length;
            count = Math.max(admitted(), pending - arrivals + departures);
            paint(now);
            wake();
        },
        destroy() {
            destroyed = true;
            cancelAnimationFrame(frame);
            observer.disconnect();
            document.removeEventListener("visibilitychange", visibility);
            for (const packet of packets) removePacket(packet);
        },
    };
}

export function colorAt(position: number, capacity = 32) {
    const hot = Math.max(0, Math.min(100, 100 - position / (capacity - 1) * 100));
    return `color-mix(in oklab,var(--bus-load-hot) ${hot.toFixed(2)}%,var(--bus-load-cool))`;
}
