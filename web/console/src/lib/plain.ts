// A browser tooltip shows the characters it is given. Agents write their
// thought summaries and goals in markdown — codex heads them with a
// **bold line** — so a tooltip fed the source reads with the asterisks
// still in it. plain is the same text with the marks taken off and the
// words left alone; it is for places that cannot render markdown, never
// for the transcript itself.
export function plain(markdown: string): string {
    return markdown
        .replace(/```+[^\n]*\n([\s\S]*?)```+/g, "$1")
        .replace(/^\s{0,3}#{1,6}\s+/gm, "")
        .replace(/^\s{0,3}>\s?/gm, "")
        .replace(/^([ \t]*)[-*+][ \t]+/gm, "$1• ")
        .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
        .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
        .replace(/(\*\*\*|___)([^\n]+?)\1/g, "$2")
        .replace(/(\*\*|__)([^\n]+?)\1/g, "$2")
        // A lone asterisk between spaces is arithmetic, not emphasis.
        .replace(/\*([^*\s][^*\n]*[^*\s]|[^*\s])\*/g, "$1")
        .replace(/(^|[\s(])_([^_\s][^_\n]*[^_\s]|[^_\s])_(?=$|[\s.,;:!?)])/gm, "$1$2")
        .replace(/~~([^\n]+?)~~/g, "$1")
        .replace(/`+([^`\n]+)`+/g, "$1")
        .replace(/[ \t]+$/gm, "")
        .replace(/\n{3,}/g, "\n\n")
        .trim();
}
