// The composer is a markdown editor rather than a textarea, so its draft is
// not an input value and its text content is the rendered form: bullets
// instead of `- `, no `**` around bold. The source lives in the editor's own
// document, which CodeMirror hangs off the content element it owns.
export const draftOf = (box) => box.evaluate((el) => el.cmTile.root.view.state.doc.toString());
