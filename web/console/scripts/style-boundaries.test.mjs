import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import ts from "../node_modules/typescript/lib/typescript.js";

const root = fileURLToPath(new URL("../src", import.meta.url));
function files(dir) {
    return readdirSync(dir, { withFileTypes: true }).flatMap((entry) =>
        entry.isDirectory() ? files(path.join(dir, entry.name)) : [path.join(dir, entry.name)]);
}
const sources = files(root);
const owners = {
    "workbench-drawer-header": "components/steve/drawer.tsx",
    "workbench-drawer-body": "components/steve/drawer.tsx",
    "workbench-panel": "components/steve/page.tsx",
    "workbench-panel-header": "components/steve/page.tsx",
    "workbench-panel-body": "components/steve/page.tsx",
    "workbench-panel-footer": "components/steve/page.tsx",
};

test("shared surfaces have one markup owner, not per-page copies", () => {
    const failures = [];
    for (const file of sources.filter((file) => file.endsWith(".tsx"))) {
        const relative = path.relative(root, file);
        const ast = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
        function visit(node) {
            if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) {
                for (const word of node.text.split(/\s+/)) {
                    if (owners[word] && owners[word] !== relative) failures.push(`${relative}: ${word} belongs to ${owners[word]}`);
                    if (word === "workbench-icon-button") failures.push(`${relative}: use IconButton instead of the retired icon style`);
                }
            }
            ts.forEachChild(node, visit);
        }
        visit(ast);
    }
    assert.deepEqual(failures, []);
});

test("page CSS cannot restyle shared component internals", () => {
    const failures = [];
    for (const file of sources.filter((file) => file.endsWith(".css"))) {
        const css = readFileSync(file, "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
        // These owners use component-local variants. No selector, regardless
        // of specificity, may take over their chrome from a parent page.
        for (const match of css.matchAll(/([^{}]+)\{/g)) {
            const selector = match[1].trim();
            if (/\.(?:workbench-panel(?:-header|-body|-footer|-toolbar)?|workbench-icon-button)\b/.test(selector)) failures.push(`${path.relative(root, file)}: ${selector}`);
            if (/\.settings-field\s+(?:label|p)\b/.test(selector)) failures.push(`${path.relative(root, file)}: field copy must not target nested control labels/values`);
        }
    }
    assert.deepEqual(failures, []);
});

test("shared presentation consumers select variants instead of overriding chrome", () => {
    const failures = [];
    for (const file of sources.filter((file) => file.endsWith(".tsx"))) {
        const ast = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
        function visit(node) {
            if ((ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) && ["Panel", "IconButton", "DialogSurface", "DialogBody", "DialogFooter"].includes(node.tagName.getText(ast))) {
                for (const attribute of node.attributes.properties) {
                    if (!ts.isJsxAttribute(attribute)) continue;
                    if (attribute.name.text === "style") failures.push(`${path.relative(root, file)}: inline style bypasses ${node.tagName.getText(ast)} variants`);
                    if (attribute.name.text !== "className" || !attribute.initializer) continue;
                    const value = ts.isStringLiteral(attribute.initializer) ? attribute.initializer.text : "";
                    for (const token of value.split(/\s+/)) {
                        if (/(?:^|:)(?:!?p[xytrblse]?-.+|rounded(?:-.+)?|shadow(?:-.+)?|ring(?:-.+)?|bg-.+|border(?:-.+)?|text-.+|font-.+|size-.+|h-.+|w-.+)$/.test(token)) {
                            failures.push(`${path.relative(root, file)}: ${node.tagName.getText(ast)} class ${token} overrides its presentation`);
                        }
                    }
                }
            }
            ts.forEachChild(node, visit);
        }
        visit(ast);
    }
    assert.deepEqual(failures, []);
});

// These consumers have ordinary icon actions and disclosures. Specialized
// controls (e.g. the code-copy success indicator) are outside this contract.
test("migrated icon actions use the accessible shared adapter", () => {
    const failures = [];
    const consumers = [
        "components/steve/sessions-tree.tsx", "components/steve/task-meta-menu.tsx",
        "components/steve/settings-editor.tsx", "components/steve/ssh-connect.tsx",
        "components/steve/machine-upgrade.tsx", "pages/skills.tsx",
    ];
    for (const relative of consumers) {
        const file = path.join(root, relative);
        const ast = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
        function visit(node) {
            if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier) && node.moduleSpecifier.text.endsWith("/button-utility")) {
                failures.push(`${relative}: business icon actions require IconButton's explicit label`);
            }
            if (ts.isJsxElement(node) && ["button", "AriaButton"].includes(node.openingElement.tagName.getText(ast))) {
                const content = node.children.filter((child) => !ts.isJsxText(child) || child.text.trim());
                const className = node.openingElement.attributes.properties.find((prop) => ts.isJsxAttribute(prop) && prop.name.text === "className")?.initializer;
                const ownChrome = className && ts.isStringLiteral(className) && (/(?:^|\s)size-\d/.test(className.text) || className.text === "conversation-arrange");
                if (ownChrome && content.length === 1 && ts.isJsxSelfClosingElement(content[0]) && /^[A-Z]/.test(content[0].tagName.getText(ast))) {
                    failures.push(`${relative}: single-icon action duplicates the shared control`);
                }
            }
            if (ts.isJsxSelfClosingElement(node) && node.tagName.getText(ast) === "Button" && node.attributes.properties.some((prop) => ts.isJsxAttribute(prop) && prop.name.text === "iconLeading")) {
                failures.push(`${relative}: icon-only Button must use IconButton`);
            }
            ts.forEachChild(node, visit);
        }
        visit(ast);
    }
    assert.deepEqual(failures, []);
});
