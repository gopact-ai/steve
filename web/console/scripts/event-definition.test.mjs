import assert from "node:assert/strict";
import test from "node:test";
import { eventDefinitionText } from "../src/lib/event-definition.ts";

test("event definitions select localized fields and retain useful fallbacks", () => {
    const definition = { version: "1", name: "Email received", description: "Default explanation", i18n: {
        "zh-CN": { name: "收到邮件", description: "邮件插件报告新邮件到达。" },
        zh: { name: "中文名称" },
        en: { name: "English name", description: "English explanation" },
    } };
    assert.deepEqual(eventDefinitionText(definition, "zh"), { name: "收到邮件", description: "邮件插件报告新邮件到达。" });
    assert.deepEqual(eventDefinitionText(definition, "ZH-cn"), { name: "收到邮件", description: "邮件插件报告新邮件到达。" });
    assert.deepEqual(eventDefinitionText(definition, "zh-TW"), { name: "中文名称", description: "邮件插件报告新邮件到达。" });
    assert.deepEqual(eventDefinitionText(definition, "fr"), { name: "Email received", description: "Default explanation" });
    assert.deepEqual(eventDefinitionText({ version: "1", i18n: { en: { description: "Only supplied text" } } }, "zh"), { description: "Only supplied text" });
    assert.deepEqual(eventDefinitionText({ version: "1", i18n: { ja: { name: "受信メール" } } }, "en"), { name: "受信メール" });
    const regional = { version: "1", name: "Native default", i18n: { de: { name: "Deutscher Name" }, "en-US": { name: "English regional name" } } };
    assert.deepEqual(eventDefinitionText(regional, "en"), { name: "English regional name" });
    assert.deepEqual(eventDefinitionText({ ...regional, name: "" }, "zh"), { name: "English regional name" });
    assert.deepEqual(eventDefinitionText({ version: "1", name: " ", description: "\n", i18n: { zh: { name: " " } } }, "zh"), {});
    assert.deepEqual(eventDefinitionText(undefined, "zh"), {});
});
