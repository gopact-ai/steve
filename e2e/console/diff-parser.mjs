import assert from "node:assert/strict";
import test from "node:test";
import { parseUnifiedDiff, splitDiffLines, splitDiffMetadata } from "../../web/console/src/lib/unified-diff.ts";

test("numbers old/new lines across replacement, addition and deletion hunks", () => {
    const patch = parseUnifiedDiff("diff --git a/app.ts b/app.ts\n--- a/app.ts\n+++ b/app.ts\n@@ -2,3 +2,4 @@ function run()\n same\n-old\n+new\n+extra\n tail\n@@ -20,0 +22,1 @@\n+added\n@@ -30,1 +32,0 @@\n-gone\n");
    assert.equal(patch.hunks.length, 3);
    assert.deepEqual(patch.hunks[0].lines.map((line) => [line.kind, line.oldLine, line.newLine]), [
        ["context", 2, 2], ["deletion", 3, undefined], ["addition", undefined, 3], ["addition", undefined, 4], ["context", 4, 5],
    ]);
    assert.equal(patch.hunks[1].lines[0].newLine, 22);
    assert.equal(patch.hunks[2].lines[0].oldLine, 30);
    assert.equal(patch.hunks[0].section, "function run()");
});

test("code beginning with +++ or --- stays content inside a hunk", () => {
    const { hunks } = parseUnifiedDiff("--- a/signs\n+++ b/signs\n@@ -1,2 +1,2 @@\n----old\n--- old\n++++new\n+++ new\n");
    assert.deepEqual(hunks[0].lines.map((line) => line.text), ["---old", "-- old", "+++new", "++ new"]);
    assert.deepEqual(hunks[0].lines.map((line) => line.kind), ["deletion", "deletion", "addition", "addition"]);
});

test("pairs replacements, retaining missing cells and newline markers", () => {
    const { hunks } = parseUnifiedDiff("@@ -1,3 +1,2 @@\n-first\n-second\n\\ No newline at end of file\n+replacement\n\\ No newline at end of file\n unchanged\n");
    const rows = splitDiffLines(hunks[0].lines);
    assert.equal(rows.length, 3);
    assert.equal(rows[0].before.text, "first");
    assert.equal(rows[0].after.text, "replacement");
    assert.equal(rows[0].after.noNewline, true);
    assert.equal(rows[1].before.text, "second");
    assert.equal(rows[1].before.noNewline, true);
    assert.equal(rows[1].after, undefined);
    assert.equal(rows[2].before, rows[2].after);
});

test("retains metadata-only patches and empty input", () => {
    const text = "diff --git a/run b/run\nold mode 100644\nnew mode 100755\n";
    assert.equal(parseUnifiedDiff(text).hunks.length, 0);
    assert.equal(parseUnifiedDiff(text).metadata.join("\n"), text.trimEnd());
    assert.deepEqual(parseUnifiedDiff(""), { hunks: [], metadata: [] });
});

test("keeps mode changes visible alongside content changes", () => {
    const patch = parseUnifiedDiff("diff --git a/run b/run\nold mode 100644\nnew mode 100755\nindex 123..456\n--- a/run\n+++ b/run\n@@ -1 +1 @@\n-old\n+new\n");
    const metadata = splitDiffMetadata(patch.metadata);
    assert.equal(patch.hunks.length, 1);
    assert.deepEqual(metadata.changes, ["old mode 100644", "new mode 100755"]);
    assert.deepEqual(metadata.headers, ["diff --git a/run b/run", "index 123..456", "--- a/run", "+++ b/run"]);
});

test("new and deleted files use only their existing side", () => {
    const addition = parseUnifiedDiff("--- /dev/null\n+++ b/new\n@@ -0,0 +1,2 @@\n+one\n+two\n\\ No newline at end of file\n");
    const deletion = parseUnifiedDiff("--- a/gone\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-one\n-two\n");
    assert.deepEqual(addition.hunks[0].lines.map((line) => [line.oldLine, line.newLine]), [[undefined, 1], [undefined, 2]]);
    assert.deepEqual(deletion.hunks[0].lines.map((line) => [line.oldLine, line.newLine]), [[1, undefined], [2, undefined]]);
    assert.ok(splitDiffLines(addition.hunks[0].lines).every((row) => !row.before && row.after));
    assert.ok(splitDiffLines(deletion.hunks[0].lines).every((row) => row.before && !row.after));
    assert.equal(addition.hunks[0].lines[1].noNewline, true);
});

test("tolerates partial hunks, incomplete headers and a final partial line", () => {
    const { hunks } = parseUnifiedDiff("@@ -100,20 +100,20 @@\n-old\n+partial");
    assert.equal(hunks.length, 1);
    assert.equal(hunks[0].lines[1].text, "partial");
    assert.equal(hunks[0].lines[1].newLine, 100);
    assert.equal(parseUnifiedDiff("diff --git a/a b/a\n@@ -10,").hunks.length, 0);
    assert.doesNotThrow(() => splitDiffLines(hunks[0].lines));
});

test("separates metadata for subsequent file sections without false content rows", () => {
    const patch = parseUnifiedDiff("@@ -1 +1 @@\n-old\n+new\ndiff --git a/b b/b\n--- a/b\n+++ b/b\n@@ -1 +1 @@\n-before\n+after\n");
    assert.equal(patch.hunks.length, 2);
    assert.equal(patch.hunks[0].lines.length, 2);
    assert.deepEqual(patch.metadata, ["diff --git a/b b/b", "--- a/b", "+++ b/b"]);
});
