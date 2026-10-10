import assert from "node:assert/strict";
import test from "node:test";
import { booleanPreference, optionChoiceKey, optionChoiceValue, optionDefaultKey, reportedOption, sessionOption, withOptionPreference } from "../src/lib/session-options.ts";

test("explicit boolean false, true and unreported do not collapse", () => {
    for (const current of ["true", "false", "", undefined, "False"]) {
        const option = sessionOption({ ID: "toggle", Name: "Toggle", Type: "boolean", Current: current });
        assert.equal(option.type, "boolean");
        assert.equal(reportedOption(option), current === "true" || current === "false" ? current : undefined);
        assert.deepEqual(option.choices, []);
    }
    assert.equal(booleanPreference("false"), true);
    for (const value of ["False", "0", "", undefined]) assert.equal(booleanPreference(value), false);
});
test("observed and session projections preserve Type and opaque IDs", () => {
    const session = sessionOption({ ID: "opaque", Name: "Opaque", Type: "select", Category: "vendor/private", Current: "false", Choices: [{ Value: "false", Label: "Literal false" }, { Value: "__none", Label: "Opaque" }] });
    const observed = sessionOption({ id: "opaque", name: "Opaque", type: "select", category: "vendor/private", current: "false", values: ["false", "__none"], choices: ["Literal false", "Opaque"] });
    assert.deepEqual(session, observed);
    assert.equal(reportedOption(session), "false");
    for (const value of ["false", "__none", "default", "", "中文 $HOME"]) {
        const key = optionChoiceKey(value);
        assert.notEqual(key, optionDefaultKey);
        assert.equal(optionChoiceValue(key), value);
    }
    assert.equal(optionChoiceValue(optionDefaultKey), undefined);
});
test("unknown or missing Type never guesses boolean from values or choices", () => {
    for (const type of [undefined, "", "future"]) {
        const option = sessionOption({ ID: "x", Name: "X", Type: type, Current: "false", Choices: [{ Value: "true", Label: "True" }] });
        assert.equal(option.type, type || undefined);
        assert.equal(option.current, "false");
    }
});
test("requested preference does not mutate reported Actual", () => {
    const option = sessionOption({ ID: "toggle", Name: "Toggle", Type: "boolean", Current: "false" });
    const preferences = { toggle: "true" };
    assert.equal(preferences.toggle, "true");
    assert.equal(reportedOption(option), "false");
});
test("a malformed primitive does not become a canonical current value", () => {
    for (const Current of [false, true, null, 0]) {
        const option = sessionOption({ ID: "toggle", Name: "Toggle", Type: "boolean", Current });
        assert.equal(option.current, undefined);
        assert.equal(reportedOption(option), undefined);
    }
});
test("opaque option IDs remain own preference keys, including prototype-looking names", () => {
    let options = { existing: "unchanged" };
    for (const id of ["__proto__", "constructor", "vendor/private", "__none"]) {
        options = withOptionPreference(options, id, "false");
        assert.equal(Object.hasOwn(options, id), true);
        assert.equal(options[id], "false");
        assert.equal(Object.hasOwn(JSON.parse(JSON.stringify(options)), id), true);
        options = withOptionPreference(options, id, undefined);
        assert.equal(Object.hasOwn(options, id), false);
        assert.equal(options.existing, "unchanged");
    }
});
