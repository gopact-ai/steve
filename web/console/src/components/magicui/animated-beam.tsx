/*
 * Adapted from Magic UI Animated Beam at commit
 * 85f97fd27b053339af74ba9dd4ca428ee25989fb:
 * https://github.com/magicuidesign/magicui/blob/85f97fd27b053339af74ba9dd4ca428ee25989fb/apps/www/registry/magicui/animated-beam.tsx
 * Local changes: share path measurement across event pulses; play once at the
 * supplied time instead of looping; report completion, render block packets,
 * and remeasure a moving queue entry point.
 *
 * MIT License
 * Copyright (c) Magic UI
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */
import { useEffect, useId, useState, type RefObject } from "react";
import { motion } from "motion/react";

export interface BeamPulse { id: string; delay: number }
interface AnimatedBeamProps {
    containerRef: RefObject<HTMLElement | null>;
    fromRef: RefObject<HTMLElement | null>;
    toRef: RefObject<HTMLElement | null>;
    curvature?: number;
    packet?: "dot" | "block";
    layoutKey?: number;
    gradientStartColor: string;
    gradientStopColor: string;
    pulses: BeamPulse[];
    onComplete: (id: string) => void;
}

export function AnimatedBeam({ containerRef, fromRef, toRef, curvature = 0, packet = "dot", layoutKey, gradientStartColor, gradientStopColor, pulses, onComplete }: AnimatedBeamProps) {
    const id = useId();
    const [pathD, setPathD] = useState("");
    const [coordinates, setCoordinates] = useState({ start: 0, end: 0 });
    const [svgDimensions, setSvgDimensions] = useState({ width: 0, height: 0 });
    // Short departure rails use the same transport speed as the long intake,
    // so a busy consumer does not smear many overlapping blocks into one blob.
    const duration = packet === "block" ? Math.min(1.8, Math.max(.25, Math.abs(coordinates.end - coordinates.start) / 200)) : 1.4;
    useEffect(() => {
        const updatePath = () => {
            if (!containerRef.current || !fromRef.current || !toRef.current) return;
            const containerRect = containerRef.current.getBoundingClientRect();
            const rectA = fromRef.current.getBoundingClientRect();
            const rectB = toRef.current.getBoundingClientRect();
            setSvgDimensions({ width: containerRect.width, height: containerRect.height });
            const startX = rectA.left - containerRect.left + rectA.width / 2;
            const startY = rectA.top - containerRect.top + rectA.height / 2;
            const endX = rectB.left - containerRect.left + rectB.width / 2;
            const endY = rectB.top - containerRect.top + rectB.height / 2;
            setCoordinates({ start: startX, end: endX });
            setPathD(`M ${startX},${startY} Q ${(startX + endX) / 2},${startY - curvature} ${endX},${endY}`);
        };
        const observer = new ResizeObserver(updatePath);
        for (const ref of [containerRef, fromRef, toRef]) if (ref.current) observer.observe(ref.current);
        updatePath();
        return () => observer.disconnect();
    }, [containerRef, fromRef, toRef, curvature, layoutKey]);

    return <svg className="bus-flow-beam" fill="none" aria-hidden="true" width={svgDimensions.width} height={svgDimensions.height} viewBox={`0 0 ${svgDimensions.width} ${svgDimensions.height}`}>
        {packet === "dot" ? <path d={pathD} stroke="var(--color-border-primary)" strokeWidth="1.5" strokeLinecap="round" /> : null}
        {pathD ? pulses.map((pulse) => <g key={pulse.id} className="bus-flow-particle">
            <path d={pathD} stroke={`url(#${id}-${pulse.id})`} strokeWidth={packet === "block" ? 1 : 2.5} strokeOpacity={packet === "block" ? .45 : 1} strokeLinecap="round" />
            {packet === "block" ? <motion.rect x="-4" y="-9" width="8" height="18" rx="1.5" fill={gradientStopColor} style={{ offsetPath: `path('${pathD}')`, offsetRotate: "0deg", filter: `drop-shadow(0 0 3px ${gradientStopColor})` }} initial={{ offsetDistance: "0%", opacity: 0 }} animate={{ offsetDistance: "100%", opacity: [0, 1, 1, 0] }} transition={{ duration, delay: pulse.delay / 1000, ease: "linear" }} /> : <motion.circle r="2.5" fill={gradientStopColor} style={{ offsetPath: `path('${pathD}')`, offsetRotate: "0deg" }} initial={{ offsetDistance: "0%", opacity: 0 }} animate={{ offsetDistance: "100%", opacity: [0, 1, 1, 0] }} transition={{ duration, delay: pulse.delay / 1000, ease: "linear" }} />}
            <defs>
                <motion.linearGradient id={`${id}-${pulse.id}`} gradientUnits="userSpaceOnUse"
                    initial={{ x1: coordinates.start, x2: coordinates.start, y1: 0, y2: 0 }}
                    animate={{ x1: [coordinates.start, coordinates.end], x2: [coordinates.start - (coordinates.end - coordinates.start) * .1, coordinates.end - (coordinates.end - coordinates.start) * .1] }}
                    transition={{ delay: pulse.delay / 1000, duration, ease: "linear", repeat: 0 }} onAnimationComplete={() => onComplete(pulse.id)}>
                    <stop stopColor={gradientStartColor} stopOpacity="0" />
                    <stop stopColor={gradientStartColor} />
                    <stop offset="32.5%" stopColor={gradientStopColor} />
                    <stop offset="100%" stopColor={gradientStopColor} stopOpacity="0" />
                </motion.linearGradient>
            </defs>
        </g>) : null}
    </svg>;
}
