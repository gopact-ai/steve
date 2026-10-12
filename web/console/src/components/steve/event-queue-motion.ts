import type { FlowParticle } from "@/hooks/use-event-flow";

const INGRESS = 500;
const HANDOFF = 120;
const CELL_STEP = 70;
const SHIFT = 260;
const RESCALE = 500;
const MIN_PITCH = 1.5;
const MIN_COLUMNS = 32;
const smooth = (value: number) => {
    const clamped = Math.max(0, Math.min(1, value));
    return clamped * clamped * (3 - 2 * clamped);
};

interface Geometry { width: number; height: number; pitch: number; inlet: number; last: number; length: number; buffer: number }
interface Packet { el: HTMLSpanElement; animation: Animation; handoff: number | null }
interface Transit { id: string; position: number; entered: number; lastFrame: number; initialSpeed: number; cruise: number; bridge: Packet | null }
interface Scheduled extends FlowParticle { at: number }
interface Shift { started: number; wait: number }
interface Density { slots: number; units: number }
interface Rescale { started: number; pitch: number; units: number }
interface Departure { packet: Packet; from: number; speed: number; launch: number; duration: number }

// React owns observed facts. This renderer owns the adaptive light grid and
// transient packets; animation frames neither commit React nor read layout.
export function createEventQueueMotion(stage: HTMLDivElement, motion: boolean, finish: (id: string) => void, scale: (units: number) => void) {
    const grid = stage.querySelector<HTMLDivElement>(".bus-flow-dots")!;
    // Head first: adding columns at the left preserves every existing light.
    const cells: HTMLSpanElement[] = [];
    let levels = new Float64Array(0), painted = new Float64Array(0), fills = new Float64Array(0);
    const transits: Transit[] = [];
    const scheduled: Scheduled[] = [];
    const packets = new Set<Packet>();
    const shifts: Shift[] = [];
    let seen = new Set<string>();
    let geometry: Geometry | null = null;
    let density: Density = { slots: MIN_COLUMNS, units: 1 }, units = 1;
    let rescale: Rescale | null = null;
    let lastExit: Departure | null = null;
    let count = 0, target = 0, shiftBefore = -1, frame = 0;
    let destroyed = false, initialized = false, lastDeparture = -Infinity, shrinkSince = 0;

    const setCells = (length: number) => {
        if (length === cells.length) return;
        const fragment = document.createDocumentFragment();
        while (cells.length < length) {
            const cell = document.createElement("span");
            cell.className = "bus-flow-cell";
            cells.push(cell); fragment.append(cell);
        }
        grid.append(fragment);
        while (cells.length > length) cells.pop()!.remove();
        levels = new Float64Array(length);
        painted = new Float64Array(length).fill(-1);
        fills = new Float64Array(length).fill(-1);
    };
    const layout = (now: number) => {
        const g = geometry!;
        const progress = rescale ? smooth((now - rescale.started) / RESCALE) : 1;
        const pitch = g.buffer / density.slots;
        g.pitch = rescale ? rescale.pitch + (pitch - rescale.pitch) * progress : pitch;
        units = rescale ? rescale.units + (density.units - rescale.units) * progress : density.units;
        g.width = g.pitch - Math.min(2, g.pitch * .22);
        g.last = g.inlet + g.buffer - g.width;
        stage.style.setProperty("--bus-cell-width", `${g.width}px`);
        stage.style.setProperty("--bus-cell-height", `${g.height}px`);
        stage.dataset.unitScale = String(units);
        if (progress === 1) { rescale = null; setCells(density.slots); }
        for (let i = 0; i < cells.length; i++) {
            const left = g.buffer - g.width - i * g.pitch;
            cells[i].style.left = `${left}px`;
            cells[i].style.width = `${g.width}px`;
            cells[i].style.setProperty("--bus-slot-color", colorAt(left / g.buffer));
            cells[i].style.setProperty("--bus-prev-color", colorAt((left - g.pitch) / g.buffer));
        }
        shiftBefore = -1;
    };
    const configure = (demand: number, now: number, immediate = false) => {
        if (!geometry) return;
        // Grow for the backlog, not the service quota. Leave room to receive
        // new events; group only below the physical width of a readable light.
        const maximum = Math.max(MIN_COLUMNS, Math.min(1024, Math.floor(geometry.buffer / MIN_PITCH)));
        const reserve = Math.max(8, Math.ceil(demand * .25));
        const nextUnits = demand + 4 <= maximum ? 1 : Math.max(1, Math.ceil((demand + reserve) / maximum));
        const nextSlots = Math.max(MIN_COLUMNS, Math.min(maximum, Math.ceil((demand + reserve) / nextUnits / 16) * 16));
        const crowded = demand + 4 > density.slots * density.units;
        const smaller = demand < density.slots * density.units * .35;
        if (!smaller) shrinkSince = 0;
        else if (!shrinkSince) shrinkSince = now;
        // Hysteresis prevents zooming in and out on every producer/consumer read.
        if (!immediate && !crowded && !(smaller && now - shrinkSince >= 8000)) return;
        if (nextSlots === density.slots && nextUnits === density.units && !immediate) return;
        rescale = motion && !immediate ? { started: now, pitch: geometry.pitch, units } : null;
        density = { slots: nextSlots, units: nextUnits };
        scale(nextUnits);
        setCells(Math.max(cells.length, nextSlots));
        layout(now);
    };
    const shiftAmount = (now: number) => {
        let amount = 0;
        // Independent deadlines keep repeated claims from sliding the pile out.
        for (let i = shifts.length - 1; i >= 0; i--) {
            const shift = shifts[i];
            const remaining = 1 - smooth((now - shift.started - shift.wait) / SHIFT);
            if (remaining === 0) shifts.splice(i, 1);
            else amount += remaining;
        }
        return amount / units;
    };
    const removePacket = (packet: Packet | null) => {
        if (!packet) return;
        packets.delete(packet);
        packet.animation.cancel();
        packet.el.remove();
    };
    const exitReady = (now: number) => {
        if (now - lastDeparture < 120) return false;
        if (!lastExit || !packets.has(lastExit.packet)) return true;
        // A newly created compositor animation can start on the next frame.
        // Its actual clock avoids releasing a follower one frame too early.
        const age = lastExit.packet.animation.currentTime;
        if (typeof age !== "number") return false;
        if (age >= lastExit.duration) return true;
        // This is the same quadratic launch as the compositor keyframes.
        const distance = lastExit.speed * (age < lastExit.launch ? age * age / (2 * lastExit.launch) : age - lastExit.launch / 2);
        const head = geometry!.last - shiftAmount(now) * geometry!.pitch;
        // Reserve a whole pitch of the widest possible light. In-flight cores
        // also grow when the grid contracts, so today's width is insufficient.
        return lastExit.from + distance - head >= geometry!.buffer / MIN_COLUMNS;
    };
    const completeTransit = (index: number) => {
        const [transit] = transits.splice(index, 1);
        removePacket(transit.bridge);
        finish(transit.id);
    };
    const admitted = () => {
        let total = 0;
        for (const item of transits) if (!item.bridge || item.bridge.handoff !== null) total++;
        return total;
    };
    const parkedCount = () => Math.max(0, count - admitted()) / units;
    const reset = () => {
        const active = new Set([...scheduled.map((item) => item.id), ...transits.map((item) => item.id), ...Array.from(packets, (packet) => packet.el.dataset.activity!)]);
        for (const packet of packets) removePacket(packet);
        active.forEach(finish);
        transits.length = 0; scheduled.length = 0; shifts.length = 0;
        lastExit = null; lastDeparture = -Infinity;
        count = target;
        configure(target, performance.now(), true);
        paint(performance.now());
    };
    const measure = () => {
        const rect = stage.getBoundingClientRect();
        const buffer = grid.getBoundingClientRect();
        if (rect.width <= 0 || buffer.width <= 0) return;
        const inlet = buffer.left - rect.left;
        // ResizeObserver also sends an initial notification. An unchanged
        // coordinate space must not cancel events received just after mount.
        if (geometry?.length === rect.width && geometry.buffer === buffer.width && geometry.inlet === inlet) return;
        geometry = { width: 0, height: 3, pitch: 0, inlet, last: 0, length: rect.width, buffer: buffer.width };
        // Window resizing changes the coordinate space; establish a new baseline.
        reset();
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
        let exitSpeed = 0, exitLaunch = 0;
        let keyframes: Keyframe[];
        if (outgoing) {
            // A shared rail speed prevents later, wider cores from catching
            // earlier departures while density changes. Resize resets the rail.
            const speed = g.buffer / MIN_COLUMNS / CELL_STEP;
            const distance = Math.min((to - from) * .4, speed * Math.max(140, 2 * g.pitch / speed) / 2);
            const launch = 2 * distance / speed;
            exitSpeed = speed; exitLaunch = launch;
            const step = g.pitch / units;
            clearance = step <= distance ? Math.sqrt(2 * step * launch / speed) : launch + (step - distance) / speed;
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
            lastExit = { packet, from, speed: exitSpeed, launch: exitLaunch, duration };
            shifts.push({ started: now, wait: clearance });
            count--; lastDeparture = now;
            packet.animation.finished.then(() => {
                if (destroyed || !packets.has(packet)) return;
                removePacket(packet); finish(item.id);
            }).catch(() => {});
        } else {
            packet.animation.finished.then(() => {
                if (destroyed || !packets.has(packet)) return;
                packet.handoff = performance.now(); count++;
            }).catch(() => {});
            transits.push({ id: item.id, position: 0, entered: now, lastFrame: now, initialSpeed: (g.inlet + g.width) / INGRESS, cruise: Math.max(g.pitch / CELL_STEP, g.buffer / 2200), bridge: packet });
        }
    };
    const distanceAt = (transit: Transit, now: number) => {
        const age = Math.max(0, now - transit.entered), u = Math.min(1, age / HANDOFF);
        const initial = transit.initialSpeed, cruise = transit.cruise;
        return age >= HANDOFF ? (initial + cruise) * HANDOFF / 2 + (age - HANDOFF) * cruise
            : initial * age + (cruise - initial) * HANDOFF * (u ** 3 - u ** 4 / 2);
    };
    const advance = (now: number) => {
        if (rescale) layout(now);
        for (let i = 0; i < scheduled.length;) {
            const item = scheduled[i];
            // A claim can share a read with its publication: wait for a real head.
            if (item.at > now || (item.direction === "out" && (parkedCount() === 0 || !exitReady(now)))) { i++; continue; }
            scheduled.splice(i, 1); spawn(item, now);
        }
        for (let i = 0; i < transits.length;) {
            const transit = transits[i];
            if (transit.bridge) {
                if (transit.bridge.handoff === null) { i++; continue; }
                if (transit.entered < transit.bridge.handoff) {
                    transit.entered = transit.bridge.handoff; transit.lastFrame = transit.entered;
                    transit.bridge.el.style.transform = `translateX(${geometry!.inlet}px)`;
                    transit.bridge.el.style.opacity = "1"; transit.bridge.animation.cancel();
                }
            }
            const shifting = shiftAmount(now), g = geometry!;
            const frontier = g.buffer - g.width - (parkedCount() + shifting) * g.pitch;
            const limit = i === 0 ? frontier : Math.min(frontier, transits[i - 1].position - g.pitch);
            const distance = distanceAt(transit, now) - distanceAt(transit, transit.lastFrame);
            transit.position = Math.max(transit.position, Math.min(Math.max(0, limit), transit.position + distance));
            transit.lastFrame = now;
            const joined = now - transit.entered >= HANDOFF;
            if (i === 0 && joined && (frontier < 0 || (transit.position >= frontier && shifting === 0))) { completeTransit(i); continue; }
            i++;
        }
        // Bounded activity is not an authoritative delivery count.
        if (!transits.length && !scheduled.length) count = target;
    };
    function paint(now: number) {
        if (!geometry) return;
        const g = geometry, parked = parkedCount();
        levels.fill(0);
        for (const transit of transits) {
            const position = Math.max(0, (g.buffer - g.width - transit.position) / g.pitch);
            const cell = Math.floor(position), fraction = position - cell;
            const appearance = transit.bridge ? (transit.bridge.handoff === null ? 0 : smooth((now - transit.entered) / HANDOFF)) : 1;
            if (cell < cells.length) levels[cell] = Math.min(1, levels[cell] + (1 - fraction) * appearance);
            if (cell + 1 < cells.length) levels[cell + 1] = Math.min(1, levels[cell + 1] + fraction * appearance);
            if (transit.bridge && transit.bridge.handoff !== null) {
                transit.bridge.el.style.transform = `translateX(${g.inlet + transit.position}px)`;
                transit.bridge.el.style.opacity = String(1 - appearance);
                transit.bridge.el.style.setProperty("--bus-packet-color", colorAt(transit.position / g.buffer));
                if (appearance === 1) { removePacket(transit.bridge); transit.bridge = null; }
            }
        }
        const offset = Math.round(shiftAmount(now) * 10000) / 10000;
        if (offset !== shiftBefore) {
            stage.style.setProperty("--bus-shift-offset", `${-offset * g.pitch}px`);
            stage.style.setProperty("--bus-shift-tint", `${Math.min(1, offset) * 100}%`);
            shiftBefore = offset;
        }
        for (let i = 0; i < cells.length; i++) {
            const fill = Math.max(0, Math.min(1, parked - i));
            if (fill !== fills[i]) {
                cells[i].dataset.filled = String(fill > 0);
                cells[i].style.setProperty("--bus-fill", String(fill)); fills[i] = fill;
            }
            const level = Math.round(levels[i] * 1000) / 1000;
            if (level !== painted[i]) { cells[i].style.setProperty("--bus-level", String(level)); painted[i] = level; }
        }
    }
    const tick = (now: number) => {
        frame = 0;
        if (destroyed || document.hidden) return;
        advance(now); paint(now);
        if (scheduled.length || transits.length || shifts.length || rescale) frame = requestAnimationFrame(tick);
    };
    const wake = () => { if (motion && geometry && !document.hidden && !frame) frame = requestAnimationFrame(tick); };
    const visibility = () => { cancelAnimationFrame(frame); frame = 0; reset(); };
    const observer = new ResizeObserver(measure);
    observer.observe(stage);
    document.addEventListener("visibilitychange", visibility);
    measure();
    return {
        sync(pending: number, particles: FlowParticle[]) {
            target = pending;
            if (!motion || document.hidden || !geometry) { reset(); particles.forEach((item) => finish(item.id)); return; }
            const now = performance.now(), retained = new Set(particles.map((item) => item.id));
            // Bound renderer activity along with the hook's 192 marks.
            for (let i = scheduled.length - 1; i >= 0; i--) if (!retained.has(scheduled[i].id)) scheduled.splice(i, 1);
            for (let i = transits.length - 1; i >= 0; i--) if (!retained.has(transits[i].id)) { removePacket(transits[i].bridge); transits.splice(i, 1); }
            for (const packet of packets) if (!retained.has(packet.el.dataset.activity!)) removePacket(packet);
            for (const item of particles) {
                if (seen.has(item.id)) continue;
                seen.add(item.id); scheduled.push({ ...item, at: now + item.delay });
            }
            seen = retained;
            const arrivals = scheduled.filter((item) => item.direction === "in").length + transits.filter((item) => item.bridge?.handoff === null).length;
            const departures = scheduled.filter((item) => item.direction === "out").length;
            count = Math.max(admitted(), pending - arrivals + departures);
            configure(Math.max(pending, count), now, !initialized); initialized = true;
            paint(now); wake();
        },
        destroy() {
            destroyed = true; cancelAnimationFrame(frame); observer.disconnect();
            document.removeEventListener("visibilitychange", visibility);
            for (const packet of packets) removePacket(packet);
            grid.replaceChildren();
        },
    };
}

function colorAt(position: number) {
    const hot = Math.max(0, Math.min(100, 100 - position * 100));
    return `color-mix(in oklab,var(--bus-load-hot) ${hot.toFixed(2)}%,var(--bus-load-cool))`;
}
