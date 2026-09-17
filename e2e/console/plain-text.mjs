import assert from "node:assert/strict";
import test from "node:test";
import { plain } from "../../web/console/src/lib/plain.ts";

test("a thought summary reads as words in a tooltip, not as its source", () => {
    const thought = "**Inspecting installation requirements**\n\nI need to inspect the mandatory AGENTS first. Maybe I should consider using `corepack`?";
    assert.equal(plain(thought), "Inspecting installation requirements\n\nI need to inspect the mandatory AGENTS first. Maybe I should consider using corepack?");
});

test("headings, quotes, bullets, links and strikethrough lose their marks and keep their text", () => {
    assert.equal(plain("## Plan\n- read *the* lockfile\n- run ~~npm~~ pnpm\n> a note\n[the docs](https://example.com/a)"),
        "Plan\n• read the lockfile\n• run npm pnpm\na note\nthe docs");
});

test("punctuation that is not markdown survives", () => {
    // Arithmetic, glob patterns and snake_case are not emphasis.
    assert.equal(plain("2 * 3 * 4 is 24"), "2 * 3 * 4 is 24");
    assert.equal(plain("run node_modules/.bin/vite with a_b_c"), "run node_modules/.bin/vite with a_b_c");
    assert.equal(plain("---"), "---");
});

test("a fenced block keeps its code and loses its fence", () => {
    assert.equal(plain("Try this:\n```sh\npnpm install\n```"), "Try this:\npnpm install");
});

test("blank edges and runs of blank lines are tidied", () => {
    assert.equal(plain("\n\n  **done**   \n\n\n\nnext\n"), "done\n\nnext");
});
