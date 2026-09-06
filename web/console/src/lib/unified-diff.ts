export interface DiffLine {
    kind: "context" | "addition" | "deletion";
    text: string;
    oldLine?: number;
    newLine?: number;
    noNewline?: boolean;
}

export interface DiffHunk {
    header: string;
    section: string;
    oldStart: number;
    oldCount: number;
    newStart: number;
    newCount: number;
    lines: DiffLine[];
}

export interface UnifiedDiff {
    hunks: DiffHunk[];
    metadata: string[];
}

// Parse the bounded unified patches supplied by the attempt API. Counts
// distinguish file headers from source lines that themselves start with +/-.
// A cut-off hunk is still useful: render every complete prefix we received.
export function parseUnifiedDiff(diff: string): UnifiedDiff {
    const result: UnifiedDiff = { hunks: [], metadata: [] };
    const rawLines = diff.split("\n");
    if (rawLines.at(-1) === "") rawLines.pop();
    let hunk: DiffHunk | undefined;
    let oldLine = 0;
    let newLine = 0;
    for (const raw of rawLines) {
        const header = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?:\s?(.*))?$/.exec(raw);
        if (header) {
            hunk = {
                header: raw,
                section: header[5] ?? "",
                oldStart: Number(header[1]), oldCount: header[2] === undefined ? 1 : Number(header[2]),
                newStart: Number(header[3]), newCount: header[4] === undefined ? 1 : Number(header[4]),
                lines: [],
            };
            oldLine = hunk.oldStart;
            newLine = hunk.newStart;
            result.hunks.push(hunk);
            continue;
        }
        if (hunk && raw.startsWith("\\ No newline at end of file")) {
            const previous = hunk.lines.at(-1);
            if (previous) previous.noNewline = true;
            continue;
        }
        if (hunk && (oldLine < hunk.oldStart + hunk.oldCount || newLine < hunk.newStart + hunk.newCount)) {
            if (raw.startsWith(" ")) {
                hunk.lines.push({ kind: "context", text: raw.slice(1), oldLine: oldLine++, newLine: newLine++ });
                continue;
            }
            if (raw.startsWith("-")) {
                hunk.lines.push({ kind: "deletion", text: raw.slice(1), oldLine: oldLine++ });
                continue;
            }
            if (raw.startsWith("+")) {
                hunk.lines.push({ kind: "addition", text: raw.slice(1), newLine: newLine++ });
                continue;
            }
        }
        hunk = undefined;
        result.metadata.push(raw);
    }
    return result;
}

export interface SplitDiffLine { before?: DiffLine; after?: DiffLine }

export function splitDiffMetadata(lines: readonly string[]): { changes: string[]; headers: string[] } {
    const changes: string[] = [];
    const headers: string[] = [];
    for (const line of lines) {
        if (!line) continue;
        // Only transport headers are optional reading. File modes and other
        // non-text changes remain visible even when the patch also has hunks.
        if (/^(?:diff --git |index |--- |\+\+\+ )/.test(line)) headers.push(line);
        else changes.push(line);
    }
    return { changes, headers };
}

// Pair adjacent deletion/addition runs by position. This is a presentation
// alignment, not a claim that two different lines contain equivalent code.
export function splitDiffLines(lines: readonly DiffLine[]): SplitDiffLine[] {
    const rows: SplitDiffLine[] = [];
    let before: DiffLine[] = [];
    let after: DiffLine[] = [];
    const flush = () => {
        for (let i = 0; i < Math.max(before.length, after.length); i++) rows.push({ before: before[i], after: after[i] });
        before = [];
        after = [];
    };
    for (const line of lines) {
        if (line.kind === "context") {
            flush();
            rows.push({ before: line, after: line });
        } else if (line.kind === "deletion") before.push(line);
        else after.push(line);
    }
    flush();
    return rows;
}
