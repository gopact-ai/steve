import { useEffect, useRef, type RefObject } from "react";
import { EditorState, Compartment, Prec } from "@codemirror/state";
import { EditorView, keymap, placeholder as placeholderText, drawSelection } from "@codemirror/view";
import { defaultKeymap, history, historyKeymap, insertNewline } from "@codemirror/commands";
import { insertNewlineContinueMarkup, markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { livePreview } from "@/lib/markdown-live";

// The composer's input. The draft is plain markdown text, the same string
// that is sent, but it is written in an editor that shows what the marks
// mean instead of the marks themselves. A textarea cannot do that: every
// weight or size it painted would move the caret away from its letter.
//
// The page holds the draft, so this stays a controlled input: text typed
// here is reported up, and text put there from elsewhere — a verb, a
// restored draft, a replaced one — is written in without losing the caret.
export interface DraftBox {
    focus(): void;
    readonly value: string;
    caretToEnd(): void;
}

export interface MarkdownInputProps {
    value: string;
    onChange: (value: string) => void;
    // The page decides what a key means: Enter sends, the arrows walk the
    // completion list. Anything it takes is marked handled and stops here.
    onKey?: (event: KeyboardEvent) => void;
    onPasteFiles?: (files: File[]) => void;
    placeholder: string;
    label: string;
    disabled?: boolean;
    handle: RefObject<DraftBox | null>;
}


export function MarkdownInput(p: MarkdownInputProps) {
    const host = useRef<HTMLDivElement | null>(null);
    const view = useRef<EditorView | null>(null);
    const live = useRef(p);
    live.current = p;
    // The Enter that confirms an input method's candidate belongs to the
    // method, not to the page, and is still held down when the composition
    // ends. Its release is what says the next Enter is the writer's own.
    const confirming = useRef(false);
    const composing = useRef(false);
    const look = useRef({ text: new Compartment(), label: new Compartment(), editable: new Compartment() });

    // What the box announces itself as. A content-editable box has no
    // disabled attribute of its own, so an unusable composer says so here.
    const described = (props: MarkdownInputProps) => EditorView.contentAttributes.of({
        "aria-label": props.label,
        "aria-multiline": "true",
        ...(props.disabled ? { "aria-disabled": "true" } : {}),
    });

    useEffect(() => {
        const compartments = look.current;
        const editor = new EditorView({
            state: EditorState.create({
                doc: live.current.value,
                extensions: [
                    history(),
                    drawSelection(),
                    EditorView.lineWrapping,
                    // The flavour people actually write: task boxes and
                    // struck-through text are markdown here as much as
                    // anywhere else in the product.
                    markdown({ base: markdownLanguage }),
                    livePreview,
                    compartments.text.of(placeholderText(live.current.placeholder)),
                    compartments.label.of(described(live.current)),
                    compartments.editable.of(EditorView.editable.of(!live.current.disabled)),
                    // Enter belongs to the page, and an input method's Enter
                    // belongs to the input method: confirming a candidate
                    // happens below the DOM, so the only thing this could add
                    // is a stray newline. Engines disagree on whether that
                    // Enter arrives before or after compositionend, so the box
                    // also swallows the one that follows a composition while
                    // the key is still down. The clock cannot tell those two
                    // Enters apart — a busy machine stretches the gap between
                    // them, and a draft would be sent half-written — but the
                    // keyboard can: a second Enter needs a release first.
                    Prec.highest(EditorView.domEventHandlers({
                        keydown: (event, editorView) => {
                            if (event.isComposing || composing.current || editorView.composing || event.keyCode === 229) {
                                if (event.key === "Enter") event.preventDefault();
                                return event.key === "Enter";
                            }
                            if (event.key === "Enter" && confirming.current) {
                                event.preventDefault();
                                return true;
                            }
                            live.current.onKey?.(event);
                            if (event.defaultPrevented) return true;
                            // A newline inside a list carries the list with it,
                            // and an item left empty ends it.
                            if (event.key === "Enter" && event.shiftKey) {
                                event.preventDefault();
                                return insertNewlineContinueMarkup(editorView) || insertNewline(editorView);
                            }
                            return false;
                        },
                        keyup: () => { confirming.current = false; return false; },
                        paste: (event) => {
                            const files = Array.from(event.clipboardData?.files ?? []);
                            if (files.length) live.current.onPasteFiles?.(files);
                            return false;
                        },
                    })),
                    keymap.of([...historyKeymap, ...defaultKeymap]),
                    EditorView.updateListener.of((update) => {
                        if (update.docChanged) live.current.onChange(update.state.doc.toString());
                    }),
                ],
            }),
            parent: host.current!,
        });
        // Composition is watched on the element itself rather than through
        // the editor's own dispatch, which drops key events mid-composition
        // and would leave this listener waiting for an end that never comes.
        const opened = () => { composing.current = true; };
        const closed = () => { composing.current = false; confirming.current = true; };
        editor.contentDOM.addEventListener("compositionstart", opened);
        editor.contentDOM.addEventListener("compositionend", closed);
        view.current = editor;
        live.current.handle.current = {
            focus: () => editor.focus(),
            get value() { return editor.state.doc.toString(); },
            caretToEnd: () => editor.dispatch({ selection: { anchor: editor.state.doc.length }, scrollIntoView: true }),
        };
        return () => {
            editor.contentDOM.removeEventListener("compositionstart", opened);
            editor.contentDOM.removeEventListener("compositionend", closed);
            editor.destroy();
            view.current = null;
            live.current.handle.current = null;
        };
    }, []);

    useEffect(() => {
        const editor = view.current;
        if (!editor) return;
        const held = editor.state.doc.toString();
        if (held === p.value) return;
        const caret = Math.min(editor.state.selection.main.anchor, p.value.length);
        editor.dispatch({
            changes: { from: 0, to: held.length, insert: p.value },
            selection: { anchor: held ? caret : p.value.length },
        });
    }, [p.value]);

    useEffect(() => {
        view.current?.dispatch({ effects: look.current.text.reconfigure(placeholderText(p.placeholder)) });
    }, [p.placeholder]);

    useEffect(() => {
        view.current?.dispatch({ effects: look.current.label.reconfigure(described(p)) });
    }, [p.label, p.disabled]);

    useEffect(() => {
        view.current?.dispatch({ effects: look.current.editable.reconfigure(EditorView.editable.of(!p.disabled)) });
    }, [p.disabled]);

    return <div ref={host} className="composer-box" data-disabled={p.disabled ? "" : undefined} />;
}
