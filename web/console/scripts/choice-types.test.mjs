import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import ts from "typescript";

test("human-request commands and selector values have independent wire types", () => {
    const file = fileURLToPath(new URL("./choice-types.test.ts", import.meta.url));
    const program = ts.createProgram([file], {
        noEmit: true, strict: true, skipLibCheck: true,
        target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext, moduleResolution: ts.ModuleResolutionKind.Bundler,
    });
    const diagnostics = ts.getPreEmitDiagnostics(program);
    assert.equal(diagnostics.length, 0, ts.formatDiagnosticsWithColorAndContext(diagnostics, {
        getCurrentDirectory: () => "", getCanonicalFileName: (file) => file, getNewLine: () => "\n",
    }));
});
