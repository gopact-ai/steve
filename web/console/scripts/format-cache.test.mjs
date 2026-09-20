import assert from "node:assert/strict";
import test from "node:test";
import { number, relative } from "../src/lib/format.ts";

test("repeated list values reuse formatters without freezing locale, options or relative time", () => {
    const originals = { NumberFormat: Intl.NumberFormat, RelativeTimeFormat: Intl.RelativeTimeFormat };
    const counts = { NumberFormat: 0, RelativeTimeFormat: 0 };
    for (const [name, Original] of Object.entries(originals)) {
        Intl[name] = new Proxy(Original, { construct(target, args) { counts[name]++; return Reflect.construct(target, args); } });
    }
    try {
        const now = Date.parse("2026-09-20T00:00:00Z");
        for (const locale of ["en", "zh"]) {
            const intlLocale = locale === "zh" ? "zh-CN" : "en";
            const expectedNumber = new originals.NumberFormat(intlLocale, { minimumFractionDigits: 1, maximumFractionDigits: 2 });
            const expectedRelative = new originals.RelativeTimeFormat(intlLocale, { numeric: "auto", style: "short" });
            for (let i = 0; i < 100; i++) {
                const options = i % 2 ? { minimumFractionDigits: 1, maximumFractionDigits: 2 } : { maximumFractionDigits: 2, minimumFractionDigits: 1 };
                assert.equal(number(i * 1.234, locale, options), expectedNumber.format(i * 1.234));
                const timestamp = new Date(now - 1000 * (10 + i % 40)).toISOString();
                assert.equal(relative(timestamp, locale, now), expectedRelative.format(-(10 + i % 40), "second"));
            }
        }
        assert.equal(counts.NumberFormat, 2, "One number formatter per locale/options, not per row or options object");
        assert.equal(counts.RelativeTimeFormat, 2, "One relative formatter per locale, not per row");
        assert.equal(number(1.25, "en", { maximumFractionDigits: 0 }), "1");
        assert.equal(number(1.25, "en", { maximumFractionDigits: 2 }), "1.25");
        assert.equal(number(12345, "en"), "12,345");
        assert.equal(number(12345, "en", {}), "12,345");
        assert.throws(() => number(1, "en", { minimumFractionDigits: 3, maximumFractionDigits: 1 }), RangeError);
        const timestamp = new Date(now).toISOString();
        const expected = new originals.RelativeTimeFormat("en", { numeric: "auto", style: "short" });
        for (const seconds of [5, 59, 60, 3599, 3600, 86399, 86400]) {
            const [amount, unit] = seconds < 60 ? [seconds, "second"] : seconds < 3600 ? [Math.round(seconds / 60), "minute"] : seconds < 86400 ? [Math.round(seconds / 3600), "hour"] : [Math.round(seconds / 86400), "day"];
            assert.equal(relative(timestamp, "en", now + seconds * 1000), expected.format(-amount, unit));
        }
        assert.equal(relative(timestamp, "en", now + 4000), "Just now");
        assert.equal(relative(timestamp, "en", now - 1000), "Just now");
        assert.equal(relative("invalid", "en", now), "—");
        assert.equal(relative("", "en", now), "");
    } finally {
        Object.assign(Intl, originals);
    }
});

test("unusual options keep native Intl semantics and transient variants do not grow the cache indefinitely", () => {
    const Original = Intl.NumberFormat;
    assert.equal(number(1.25, "en", Object.create({ maximumFractionDigits: 0 })), "1");
    const hidden = {};
    Object.defineProperty(hidden, "maximumFractionDigits", { value: 0 });
    assert.equal(number(1.25, "en", hidden), "1");
    let digits = 0;
    const accessor = { get maximumFractionDigits() { return digits; } };
    assert.equal(number(1.25, "en", accessor), "1");
    digits = 2;
    assert.equal(number(1.25, "en", accessor), "1.25");
    assert.throws(() => number(1, "en", { maximumFractionDigits: NaN }), RangeError);
    assert.throws(() => number(1, "en", { maximumFractionDigits: Infinity }), RangeError);

    let created = 0;
    Intl.NumberFormat = new Proxy(Original, { construct(target, args) { created++; return Reflect.construct(target, args); } });
    try {
        const sentinel = { notation: "scientific", signDisplay: "always", minimumSignificantDigits: 3 };
        const expected = number(12.345, "en", sentinel);
        assert.equal(number(12.345, "en", { ...sentinel }), expected);
        assert.equal(created, 1, "Repeated options reuse an instance");
        for (const locale of ["en", "zh"]) {
            for (let digits = 1; digits <= 21; digits++) number(1, locale, { minimumIntegerDigits: digits, useGrouping: false });
        }
        const before = created;
        assert.equal(number(12.345, "en", sentinel), expected);
        assert.equal(created, before + 1, "Old variants are evicted instead of retained indefinitely");
    } finally {
        Intl.NumberFormat = Original;
    }
});
