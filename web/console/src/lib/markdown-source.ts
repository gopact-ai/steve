// What a person types into the composer is markdown, and the box should
// say so while they type it. This module reads the draft as source: it
// hands back the same characters, sliced into runs with a name, so the
// composer can paint structure behind the caret.
//
// The one rule that shapes everything here: the painted layer sits under
// a real textarea, so it may only ever differ from it in ways that do not
// move a glyph. Colour, background, underline and a stroked faux-bold are
// safe; a larger heading, a real bold face or an italic are not, because
// the caret would then sit beside the letter it is supposed to be in.
// Marks are dimmed rather than hidden for the same reason.

export interface Segment {
    text: string;
    kind?: string;
}

const FENCE = /^([ \t]*)(`{3,}|~{3,})([^\n]*)$/;
const HEADING = /^([ \t]{0,3})(#{1,6})([ \t]+)([^\n]*)$/;
const QUOTE = /^([ \t]{0,3})((?:>[ \t]?)+)([^\n]*)$/;
const ITEM = /^([ \t]*)([-*+]|\d{1,9}[.)])([ \t]+)(\[[ xX]\][ \t]+)?([^\n]*)$/;
const RULE = /^[ \t]{0,3}([-*_])[ \t]*(?:\1[ \t]*){2,}$/;

interface Inline {
    re: RegExp;
    // What may stand to the left and right of the match. Underscores are
    // the reason: node_modules and snake_case are words, not emphasis.
    before?: RegExp;
    after?: RegExp;
    parts: (m: RegExpExecArray) => [string, string?][];
}

const wrapped = (kind: string) => (m: RegExpExecArray): [string, string?][] => [[m[1], "md-mark"], [m[2], kind], [m[1], "md-mark"]];

const INLINE: Inline[] = [
    { re: /(`+)([^`\n]*?)\1/y, parts: wrapped("md-code") },
    {
        re: /(!?\[)([^\]\n]*)(\]\()([^)\s\n]*)((?:[ \t]+"[^"\n]*")?)(\))/y,
        parts: (m) => [[m[1], "md-mark"], [m[2], "md-link"], [m[3], "md-mark"], [m[4] + m[5], "md-url"], [m[6], "md-mark"]],
    },
    { re: /(https?:\/\/[^\s<>()]+)/y, parts: (m) => [[m[1], "md-url"]] },
    { re: /(\*\*\*|___)(\S(?:[^\n]*?\S)?)\1/y, parts: wrapped("md-strong md-em") },
    { re: /(\*\*|__)(\S(?:[^\n]*?\S)?)\1/y, parts: wrapped("md-strong") },
    { re: /(~~)(\S(?:[^\n]*?\S)?)\1/y, parts: wrapped("md-strike") },
    // A lone asterisk between spaces is arithmetic, not emphasis.
    { re: /(\*)([^*\s](?:[^*\n]*[^*\s])?)\1/y, parts: wrapped("md-em") },
    { re: /(_)([^_\s](?:[^_\n]*[^_\s])?)\1/y, before: /[\s("'«]/, after: /[\s.,;:!?)"'»]/, parts: wrapped("md-em") },
];

export function markdownSegments(source: string): Segment[] {
    const out: Segment[] = [];
    const push = (text: string, kind?: string) => {
        if (!text) return;
        const last = out[out.length - 1];
        if (last && last.kind === kind) last.text += text;
        else out.push(kind ? { text, kind } : { text });
    };

    const inline = (text: string, base?: string) => {
        const named = (kind?: string) => [base, kind].filter(Boolean).join(" ") || undefined;
        let i = 0;
        let plain = "";
        while (i < text.length) {
            let hit: [string, string?][] | null = null;
            let width = 0;
            for (const rule of INLINE) {
                if (rule.before && i > 0 && !rule.before.test(text[i - 1])) continue;
                rule.re.lastIndex = i;
                const m = rule.re.exec(text);
                if (!m) continue;
                const after = text[i + m[0].length];
                if (rule.after && after !== undefined && !rule.after.test(after)) continue;
                hit = rule.parts(m);
                width = m[0].length;
                break;
            }
            if (!hit) {
                plain += text[i];
                i += 1;
                continue;
            }
            push(plain, named());
            plain = "";
            for (const [part, kind] of hit) push(part, named(kind));
            i += width;
        }
        push(plain, named());
    };

    let fence: string | null = null;
    source.split("\n").forEach((line, index) => {
        if (index) push("\n");
        if (fence !== null) {
            const close = FENCE.exec(line);
            if (close && close[2][0] === fence[0] && close[2].length >= fence.length) {
                fence = null;
                push(line, "md-fence");
            } else push(line, "md-code-block");
            return;
        }
        const open = FENCE.exec(line);
        if (open) {
            fence = open[2];
            push(line, "md-fence");
            return;
        }
        if (RULE.test(line)) return push(line, "md-rule");
        const heading = HEADING.exec(line);
        if (heading) {
            push(heading[1]);
            push(heading[2] + heading[3], "md-mark");
            return inline(heading[4], "md-heading");
        }
        const quote = QUOTE.exec(line);
        if (quote) {
            push(quote[1]);
            push(quote[2], "md-mark md-quote");
            return inline(quote[3], "md-quote");
        }
        const item = ITEM.exec(line);
        if (item) {
            push(item[1]);
            push(item[2] + item[3], "md-bullet");
            if (item[4]) push(item[4], /\[[xX]\]/.test(item[4]) ? "md-ticked" : "md-box");
            return inline(item[5]);
        }
        inline(line);
    });
    return out;
}

// Pressing newline in the middle of a list should keep the list going,
// the way it does in every editor people write markdown in. An item with
// nothing in it means the list is over: the marker is taken back instead
// of repeated, which is how a list is left without reaching for Backspace.
export interface Continuation {
    // What replaces the caret's line up to the caret. An empty string
    // clears a marker that was never filled in.
    insert: string;
}

export function listContinuation(line: string): Continuation | null {
    const quote = QUOTE.exec(line);
    if (quote && !ITEM.exec(quote[3])) {
        const marker = quote[1] + quote[2];
        return { insert: quote[3].trim() ? "\n" + marker : "" };
    }
    const nested = quote ? quote[1] + quote[2] : "";
    const rest = quote ? quote[3] : line;
    const item = ITEM.exec(rest);
    if (!item) return null;
    const empty = !item[5].trim();
    if (empty) return { insert: "" };
    const marker = /^\d/.test(item[2])
        ? String(Number(item[2].slice(0, -1)) + 1) + item[2].slice(-1)
        : item[2];
    const box = item[4] ? item[4].replace(/\[[xX]\]/, "[ ]") : "";
    return { insert: "\n" + nested + item[1] + marker + item[3] + box };
}
