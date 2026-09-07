import type { MaterialSelector } from "./types";

type PickedText = { selector: MaterialSelector; excerpt: string };
type Boundary = { element: HTMLElement; offset: number };
const maxRaw = 2 << 20;
const maxQuote = 64 << 10;
const maxNodes = 20_000;

function inside(range: Range, root: HTMLElement): boolean {
    return !range.collapsed && root.contains(range.startContainer) && root.contains(range.endContainer);
}

function element(node: Node): HTMLElement | null {
    return node.nodeType === 1 ? node as HTMLElement : node.parentElement;
}

function textOffset(root: HTMLElement, node: Node, offset: number): number | null {
    try {
        const prefix = root.ownerDocument.createRange();
        prefix.selectNodeContents(root);
        prefix.setEnd(node, offset);
        return prefix.toString().length;
    } catch { return null; }
}

function texts(range: Range, root: HTMLElement): Text[] | null {
    const walker = root.ownerDocument.createTreeWalker(root, 0xFFFFFFFF);
    const selected: Text[] = [];
    let count = 0;
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (++count > maxNodes) return null;
        if (node.nodeType !== 3) continue;
        if (!range.intersectsNode(node)) continue;
        const start = node === range.startContainer ? range.startOffset : 0;
        const end = node === range.endContainer ? range.endOffset : (node.textContent?.length || 0);
        if (end > start) {
            if (element(node)?.closest("[data-selection-ignore]")) return null;
            selected.push(node as Text);
        }
    }
    return selected;
}

function markedElements(root: HTMLElement, selector: string): HTMLElement[] | null {
    const result: HTMLElement[] = root.matches(selector) ? [root] : [];
    const walker = root.ownerDocument.createTreeWalker(root, 1);
    let count = 0;
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (++count > maxNodes) return null;
        if ((node as HTMLElement).matches(selector)) result.push(node as HTMLElement);
    }
    return result;
}

function boundary(range: Range, root: HTMLElement, selector: string, end: boolean, selected: Text[]): Boundary | null {
    const node = end ? range.endContainer : range.startContainer;
    const offset = end ? range.endOffset : range.startOffset;
    const direct = element(node)?.closest<HTMLElement>(selector);
    if (direct && root.contains(direct)) {
        const at = textOffset(direct, node, offset);
        return at === null ? null : { element: direct, offset: at };
    }
    const text = end ? selected[selected.length - 1] : selected[0];
    const marked = text?.parentElement?.closest<HTMLElement>(selector);
    if (!text || !marked || !root.contains(marked)) return null;
    const at = textOffset(marked, text, end ? (text === node ? offset : text.length) : (text === node ? offset : 0));
    return at === null ? null : { element: marked, offset: at };
}

function integer(raw: string | undefined): number | null {
    if (!raw || !/^\d+$/.test(raw)) return null;
    const value = Number(raw);
    return Number.isSafeInteger(value) ? value : null;
}

function unicodeBoundary(text: string, offset: number): boolean {
    return !(offset > 0 && offset < text.length && /[\uD800-\uDBFF]/.test(text[offset - 1]) && /[\uDC00-\uDFFF]/.test(text[offset]));
}

function quote(raw: string, start: number, end: number, excerpt: string): PickedText | null {
    if (start < 0 || end <= start || end > raw.length || end - start > maxQuote || !unicodeBoundary(raw, start) || !unicodeBoundary(raw, end)) return null;
    const value = raw.slice(start, end);
    if (!value.trim() || new TextEncoder().encode(value).length > maxQuote) return null;
    return { selector: { kind: "quote", quote: value }, excerpt };
}

// Match a leaf's exact CommonMark text transformation. Entity/escape tokens
// are indivisible: a partial decoded token has no exact source boundary.
function markdownOffset(source: string, rendered: string, wanted: number, kind: string, doc: Document): number | null {
    if (wanted < 0 || wanted > rendered.length || !unicodeBoundary(rendered, wanted)) return null;
    if (source === rendered) return wanted;
    let first = 0, last = source.length;
    if (kind === "inline-code") {
        const fence = /^`+/.exec(source)?.[0];
        if (!fence || !source.endsWith(fence) || source.length < fence.length * 2) return null;
        first = fence.length; last -= fence.length;
        const value = source.slice(first, last).replace(/\r\n|\r|\n/g, " ");
        if (value.startsWith(" ") && value.endsWith(" ") && /[^ ]/.test(value)) {
            first += source.slice(first, first + 2) === "\r\n" ? 2 : 1;
            last -= source.slice(last - 2, last) === "\r\n" ? 2 : 1;
        }
    }
    let at = first, output = 0, result: number | null = null;
    const decoder = doc.createElement("textarea");
    while (at < last) {
        if (output === wanted) result = at;
        const code = source.codePointAt(at)!;
        let value = String.fromCodePoint(code), length = value.length;
        if (source[at] === "\r" || source[at] === "\n") {
            length = source.slice(at, at + 2) === "\r\n" ? 2 : 1;
            value = kind === "inline-code" ? " " : "\n";
        } else if (kind === "text" && source[at] === "\\") {
            const next = source.charCodeAt(at + 1);
            if ((next >= 33 && next <= 47) || (next >= 58 && next <= 64) || (next >= 91 && next <= 96) || (next >= 123 && next <= 126)) { length = 2; value = source[at + 1]; }
        } else if (kind === "text" && source[at] === "&") {
            const entity = /^&(?:#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});/.exec(source.slice(at, Math.min(last, at + 40)))?.[0];
            if (entity) {
                decoder.innerHTML = entity;
                if (decoder.value !== entity) { value = decoder.value; length = entity.length; }
            }
        }
        if (kind === "text") value = value.replace(/\r\n|\r/g, "\n");
        if (!rendered.startsWith(value, output) || at + length > last) return null;
        output += value.length; at += length;
    }
    if (output !== rendered.length) return null;
    return wanted === output ? last : result;
}

export function selectionForReply(range: Range, root: HTMLElement, raw: string): PickedText | null {
    if (!inside(range, root) || raw.length > maxRaw) return null;
    const selected = texts(range, root), excerpt = range.toString();
    if (!selected?.length || !excerpt.trim() || excerpt.length > maxQuote) return null;
    const exact = raw.indexOf(excerpt);
    if (exact >= 0) return quote(raw, exact, exact + excerpt.length, excerpt);
    const selector = "[data-md-start][data-md-end]";
    const start = boundary(range, root, selector, false, selected), end = boundary(range, root, selector, true, selected);
    if (!start || !end) return null;
    let previous = -1;
    const checked = new Set<HTMLElement>();
    for (const text of selected) {
        const marked = text.parentElement?.closest<HTMLElement>(selector);
        if (!marked) { if (text.data.trim()) return null; continue; }
        if (checked.has(marked)) continue;
        const offset = integer(marked.dataset.mdStart), limit = integer(marked.dataset.mdEnd);
        if (offset === null || limit === null || offset < previous || limit < offset || limit > raw.length || markdownOffset(raw.slice(offset, limit), marked.textContent || "", 0, marked.dataset.mdKind || "text", root.ownerDocument) === null) return null;
        previous = limit;
        checked.add(marked);
    }
    const resolve = (point: Boundary) => {
        const first = integer(point.element.dataset.mdStart), last = integer(point.element.dataset.mdEnd);
        if (first === null || last === null || first > last || last > raw.length) return null;
        const offset = markdownOffset(raw.slice(first, last), point.element.textContent || "", point.offset, point.element.dataset.mdKind || "text", root.ownerDocument);
        return offset === null ? null : first + offset;
    };
    const from = resolve(start), to = resolve(end);
    return from === null || to === null ? null : quote(raw, from, to, excerpt);
}

function sourceLines(raw: string): { start: number; text: string }[] {
    const result: { start: number; text: string }[] = [];
    const separator = /\r\n|\r|\n/g;
    let from = 0;
    for (let match = separator.exec(raw); match; match = separator.exec(raw)) {
        if (result.length >= maxNodes) return [];
        result.push({ start: from, text: raw.slice(from, match.index) });
        from = match.index + match[0].length;
    }
    result.push({ start: from, text: raw.slice(from) });
    return result;
}

export function selectionForSource(range: Range, root: HTMLElement, raw: string): PickedText | null {
    if (!inside(range, root) || raw.length > maxRaw) return null;
    const selected = texts(range, root);
    if (!selected?.length) return null;
    const first = boundary(range, root, "[data-selection-code]", false, selected), last = boundary(range, root, "[data-selection-code]", true, selected);
    if (!first || !last) return null;
    const lines = sourceLines(raw);
    const matches = (code: HTMLElement, text: string) => code.textContent === text || (text === "" && code.textContent === "\n");
    const rows = markedElements(root, "[data-selection-line]");
    if (!rows) return null;
    const touched = rows.filter((line) => range.intersectsNode(line));
    let previous = 0;
    for (const row of touched) {
        const number = integer(row.dataset.selectionLine), code = row.querySelector<HTMLElement>("[data-selection-code]");
        if (!number || (previous && number !== previous + 1) || !lines[number - 1] || !code || !matches(code, lines[number - 1].text)) return null;
        previous = number;
    }
    const resolve = (point: Boundary) => {
        const number = integer(point.element.closest<HTMLElement>("[data-selection-line]")?.dataset.selectionLine);
        const line = number ? lines[number - 1] : undefined;
        if (!line || !matches(point.element, line.text)) return null;
        if (line.text === "" && point.offset === 1 && point.element.textContent === "\n") return number ? lines[number]?.start ?? null : null;
        if (point.offset > line.text.length) return null;
        return line.start + point.offset;
    };
    const start = resolve(first), end = resolve(last);
    return start === null || end === null ? null : quote(raw, start, end, raw.slice(start, end));
}

export function selectionForDiff(range: Range, root: HTMLElement): (PickedText & { side: "before" | "after" }) | null {
    if (!inside(range, root)) return null;
    const selected = texts(range, root);
    if (!selected?.length) return null;
    const selector = "code[data-selection-before],code[data-selection-after]";
    let first = boundary(range, root, selector, false, selected), last = boundary(range, root, selector, true, selected);
    if (!first || !last) return null;
    const marked = markedElements(root, selector);
    if (!marked) return null;
    const rows = marked.filter((node) => range.intersectsNode(node));
    if (first.element !== last.element && first.offset === first.element.textContent?.length && rows[0] === first.element) { rows.shift(); if (!rows.length) return null; first = { element: rows[0], offset: 0 }; }
    if (first.element !== last.element && last.offset === 0 && rows[rows.length - 1] === last.element) { rows.pop(); if (!rows.length) return null; last = { element: rows[rows.length - 1], offset: rows[rows.length - 1].textContent?.length || 0 }; }
    // Diff references are full source lines; a partial word selection expands
    // to its containing rows, which the action's line-range label discloses.
    if (!rows.length || rows[0] !== first.element || rows[rows.length - 1] !== last.element) return null;
    for (const side of ["after", "before"] as const) {
        const numbers = rows.map((row) => integer(side === "before" ? row.dataset.selectionBefore : row.dataset.selectionAfter));
        if (numbers.some((number, i) => number === null || number < 1 || (i > 0 && number !== numbers[i - 1]! + 1))) continue;
        const excerpt = rows.map((row) => row.textContent || "").join("\n");
        if (!excerpt.trim() || new TextEncoder().encode(excerpt).length > maxQuote) return null;
        return { side, selector: { kind: "lines", start: numbers[0]!, end: numbers[numbers.length - 1]! }, excerpt };
    }
    return null;
}
