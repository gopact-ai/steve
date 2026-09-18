// Live preview for the composer: the draft stays markdown, but while it is
// being written it is shown the way it will read once it is sent. Headings
// carry weight, bold is bold, a bullet is a bullet, code is monospaced.
//
// The marks themselves are hidden — except on the line the caret is on,
// which shows its source so the marks can still be typed and repaired.
// That is the trade every live preview makes: the line you are editing is
// source, the lines you are not are the document.
import { syntaxTree } from "@codemirror/language";
import { Decoration, EditorView, ViewPlugin, WidgetType, type DecorationSet, type ViewUpdate } from "@codemirror/view";
import type { Extension, Range } from "@codemirror/state";

class Glyph extends WidgetType {
    constructor(readonly text: string, readonly cls: string) { super(); }
    eq(other: Glyph) { return other.text === this.text && other.cls === this.cls; }
    toDOM() {
        const span = document.createElement("span");
        span.className = this.cls;
        span.textContent = this.text;
        return span;
    }
    ignoreEvent() { return false; }
}

const hide = Decoration.replace({});
const bullet = Decoration.replace({ widget: new Glyph("•", "cm-md-glyph cm-md-dot") });
const unticked = Decoration.replace({ widget: new Glyph("☐", "cm-md-glyph cm-md-box") });
const ticked = Decoration.replace({ widget: new Glyph("☑", "cm-md-glyph cm-md-box cm-md-box-on") });
const rule = Decoration.replace({ widget: new Glyph("", "cm-md-glyph cm-md-hr") });

const marked: Record<string, Decoration> = {
    StrongEmphasis: Decoration.mark({ class: "cm-md-strong" }),
    Emphasis: Decoration.mark({ class: "cm-md-em" }),
    Strikethrough: Decoration.mark({ class: "cm-md-strike" }),
    InlineCode: Decoration.mark({ class: "cm-md-code" }),
    Link: Decoration.mark({ class: "cm-md-link" }),
    URL: Decoration.mark({ class: "cm-md-url" }),
};

const lined: Record<string, Decoration> = {
    ATXHeading1: Decoration.line({ class: "cm-md-line cm-md-h1" }),
    ATXHeading2: Decoration.line({ class: "cm-md-line cm-md-h2" }),
    ATXHeading3: Decoration.line({ class: "cm-md-line cm-md-h3" }),
    ATXHeading4: Decoration.line({ class: "cm-md-line cm-md-h4" }),
    ATXHeading5: Decoration.line({ class: "cm-md-line cm-md-h5" }),
    ATXHeading6: Decoration.line({ class: "cm-md-line cm-md-h6" }),
    SetextHeading1: Decoration.line({ class: "cm-md-line cm-md-h1" }),
    SetextHeading2: Decoration.line({ class: "cm-md-line cm-md-h2" }),
};

// Hiding a mark is the one thing that may not happen under the caret, or
// the person would be editing characters they cannot see.
function build(view: EditorView): DecorationSet {
    const { state } = view;
    const ranges: Range<Decoration>[] = [];
    const editing = new Set<number>();
    for (const range of state.selection.ranges) {
        const first = state.doc.lineAt(range.from).number, last = state.doc.lineAt(range.to).number;
        for (let n = first; n <= last; n++) editing.add(n);
    }
    const source = (at: number) => editing.has(state.doc.lineAt(at).number);
    // A mark is hidden unless the caret is on its line — except the ones
    // that stand for structure rather than formatting. A bullet is what a
    // list item looks like at every moment, so `- ` becomes a bullet as it
    // is typed and stays one while the item is being written.
    const conceal = (from: number, to: number, as: Decoration = hide, always = false) => {
        if (from < to && (always || !source(from))) ranges.push(as.range(from, to));
    };
    const lines = (from: number, to: number, as: Decoration) => {
        for (let at = from; at <= to;) {
            const line = state.doc.lineAt(at);
            ranges.push(as.range(line.from));
            at = line.to + 1;
        }
    };

    for (const { from, to } of view.visibleRanges) {
        syntaxTree(state).iterate({
            from, to,
            enter: (node) => {
                const name = node.name;
                if (lined[name]) ranges.push(lined[name].range(state.doc.lineAt(node.from).from));
                else if (name === "Blockquote") lines(node.from, node.to, Decoration.line({ class: "cm-md-line cm-md-quote" }));
                else if (name === "FencedCode" || name === "CodeBlock") lines(node.from, node.to, Decoration.line({ class: "cm-md-line cm-md-code-line" }));
                else if (name === "ListItem") ranges.push(Decoration.line({ class: "cm-md-line cm-md-item" }).range(state.doc.lineAt(node.from).from));
                else if (marked[name]) ranges.push(marked[name].range(node.from, node.to));
                if (name === "HeaderMark") {
                    // The hashes and the space after them.
                    const after = state.doc.sliceString(node.to, node.to + 1) === " " ? node.to + 1 : node.to;
                    conceal(node.from, after);
                } else if (name === "QuoteMark" || name === "EmphasisMark" || name === "CodeMark" || name === "StrikethroughMark" || name === "LinkMark" || name === "URL" || name === "LinkTitle") {
                    const after = name === "QuoteMark" && state.doc.sliceString(node.to, node.to + 1) === " " ? node.to + 1 : node.to;
                    conceal(node.from, after);
                } else if (name === "ListMark") {
                    const ordered = /\d/.test(state.doc.sliceString(node.from, node.to));
                    const after = state.doc.sliceString(node.to, node.to + 1) === " " ? node.to + 1 : node.to;
                    // A task carries its own box; a bullet in front of it
                    // would only say the same thing twice.
                    const task = /^\[[ xX]\]/.test(state.doc.sliceString(after, after + 3));
                    if (!ordered) conceal(node.from, after, task ? hide : bullet, true);
                } else if (name === "TaskMarker") {
                    const after = state.doc.sliceString(node.to, node.to + 1) === " " ? node.to + 1 : node.to;
                    conceal(node.from, after, /[xX]/.test(state.doc.sliceString(node.from, node.to)) ? ticked : unticked, true);
                } else if (name === "HorizontalRule") {
                    conceal(node.from, node.to, rule);
                }
            },
        });
    }
    return Decoration.set(ranges, true);
}

export const livePreview: Extension = ViewPlugin.fromClass(
    class {
        decorations: DecorationSet;
        constructor(view: EditorView) { this.decorations = build(view); }
        update(update: ViewUpdate) {
            if (update.docChanged || update.viewportChanged || update.selectionSet) this.decorations = build(update.view);
        }
    },
    {
        decorations: (plugin) => plugin.decorations,
        // Arrow keys step over a hidden mark rather than into the middle of it.
        provide: (plugin) => EditorView.atomicRanges.of((view) => view.plugin(plugin)?.decorations ?? Decoration.none),
    },
);
