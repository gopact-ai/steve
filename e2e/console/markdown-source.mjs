import assert from "node:assert/strict";
import test from "node:test";
import { listContinuation, markdownSegments } from "../../web/console/src/lib/markdown-source.ts";

const rendered = (source) => markdownSegments(source).map((s) => (s.kind ? `${s.kind}:${s.text}` : s.text)).join("|");
const kindOf = (source, text) => markdownSegments(source).filter((s) => s.text === text).map((s) => s.kind);

// The paint sits under a real textarea. If it ever drops or adds a single
// character the caret stops standing where its letter is, so this is the
// property that matters more than any individual colour.
test("the painted layer is the draft, character for character", () => {
    const drafts = [
        "", "plain words", "- a\n- b\n", "# Heading **now**\n\n> quoted `code`\n",
        "```sh\nnpm run build\n```\ntail", "2 * 3 * 4 is 24", "run node_modules/.bin/vite with a_b_c",
        "1. one\n2. two\n\n- [ ] todo\n- [x] done\n", "[docs](https://example.com/a) and https://example.com/b",
        "***all*** ~~gone~~ _quiet_ ***", "#nothashtag\n#### four\n---\n",
    ];
    for (const draft of drafts) assert.equal(markdownSegments(draft).map((s) => s.text).join(""), draft, JSON.stringify(draft));
});

test("structure is named so it can be painted", () => {
    assert.equal(rendered("# Title"), "md-mark:# |md-heading:Title");
    assert.equal(rendered("- item"), "md-bullet:- |item");
    assert.equal(rendered("> said"), "md-mark md-quote:> |md-quote:said");
    assert.deepEqual(kindOf("**bold** text", "bold"), ["md-strong"]);
    assert.deepEqual(kindOf("a `span` here", "span"), ["md-code"]);
    assert.deepEqual(kindOf("[docs](https://example.com)", "docs"), ["md-link"]);
    assert.deepEqual(kindOf("[docs](https://example.com)", "https://example.com"), ["md-url"]);
    assert.deepEqual(kindOf("~~dropped~~", "dropped"), ["md-strike"]);
    assert.deepEqual(kindOf("- [x] done", "[x] "), ["md-ticked"]);
});

test("punctuation that is not markdown is left alone", () => {
    // The same arithmetic, globs and snake_case that tooltips already respect.
    assert.equal(rendered("2 * 3 * 4 is 24"), "2 * 3 * 4 is 24");
    assert.equal(rendered("node_modules/.bin/vite a_b_c"), "node_modules/.bin/vite a_b_c");
    assert.equal(rendered("#hashtag"), "#hashtag");
});

test("a fence holds its lines until it closes", () => {
    assert.equal(rendered("```js\nconst **a** = 1;\n```\nafter"),
        "md-fence:```js|\n|md-code-block:const **a** = 1;|\n|md-fence:```|\nafter");
    assert.equal(rendered("```\nstill open"), "md-fence:```|\n|md-code-block:still open");
});

test("a newline inside a list carries the list with it", () => {
    assert.deepEqual(listContinuation("- item"), { insert: "\n- " });
    assert.deepEqual(listContinuation("    * nested"), { insert: "\n    * " });
    assert.deepEqual(listContinuation("3. third"), { insert: "\n4. " });
    assert.deepEqual(listContinuation("3) third"), { insert: "\n4) " });
    assert.deepEqual(listContinuation("- [x] done"), { insert: "\n- [ ] " });
    assert.deepEqual(listContinuation("> quoted"), { insert: "\n> " });
    assert.deepEqual(listContinuation("> - both"), { insert: "\n> - " });
    assert.equal(listContinuation("ordinary line"), null);
    assert.equal(listContinuation(""), null);
});

test("an item left empty ends the list instead of repeating itself", () => {
    // The marker is taken back, so a list is left without reaching for Backspace.
    assert.deepEqual(listContinuation("- "), { insert: "" });
    assert.deepEqual(listContinuation("  1. "), { insert: "" });
    assert.deepEqual(listContinuation("- [ ] "), { insert: "" });
    assert.deepEqual(listContinuation("> "), { insert: "" });
});
