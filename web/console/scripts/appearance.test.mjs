import assert from "node:assert/strict";
import test from "node:test";
import {
  defaultAppearance,
  migrateTheme,
  normalizeAppearance,
  choosePalette,
  resolveAppearance,
  fontStack,
  importPalette,
  exportPalette,
  contrastRatio,
  removePalette,
} from "../src/lib/appearance.ts";
import { PALETTES } from "../src/lib/themes.ts";
test("mode and per-scheme palettes remain independent", () => {
  let a = defaultAppearance();
  a = choosePalette(a, "light", "one-light");
  a = choosePalette(a, "dark", "nord");
  assert.equal(a.mode, "system");
  assert.equal(resolveAppearance(a, false).palette.id, "one-light");
  assert.equal(resolveAppearance(a, true).palette.id, "nord");
  assert.equal(
    resolveAppearance({ ...a, mode: "light" }, true).palette.id,
    "one-light",
  );
});
test("old choice is adopted without erasing the other scheme", () => {
  const a = migrateTheme("dracula");
  assert.equal(a.mode, "dark");
  assert.equal(a.dark, "dracula");
  assert.equal(a.light, "light");
  assert.equal(migrateTheme(null).mode, "system");
});
test("invalid persistence cannot inject CSS or unbound sizes", () => {
  const a = defaultAppearance();
  a.fonts.ui = { family: "bad; color:red", size: 200 };
  a.light = "nord";
  const clean = normalizeAppearance(a);
  assert.equal(clean.fonts.ui.size, 14);
  assert.equal(clean.fonts.ui.family, "system");
  assert.equal(clean.light, "light");
  assert.deepEqual(normalizeAppearance({ version: 999 }), defaultAppearance());
  assert.ok(
    fontStack("Some Local Font", "ui").startsWith('"Some Local Font",'),
  );
  assert.ok(!fontStack("url(https://bad)", "ui").includes("url("));
});
test("palette import roundtrips colours but never carries arbitrary CSS", () => {
  const p = { ...PALETTES[0], id: "custom-test", name: "My palette" };
  const imported = importPalette(exportPalette(p));
  assert.deepEqual(imported.colors, p.colors);
  assert.equal(imported.name, p.name);
  assert.throws(() =>
    importPalette(
      '{"version":1,"palette":{"name":"bad","scheme":"dark","colors":{"bg":"url(https://bad)"}}}',
    ),
  );
  assert.throws(() => importPalette(" ".repeat(65537)));
  assert.throws(() =>
    importPalette(JSON.stringify({ version: 2, palette: p })),
  );
});
test("removed custom palette falls back only on its own scheme", () => {
  const p = { ...PALETTES[0], id: "custom-test" };
  const a = {
    ...defaultAppearance(),
    dark: p.id,
    light: "one-light",
    custom: [p],
  };
  const next = removePalette(a, p.id);
  assert.equal(next.dark, "dark");
  assert.equal(next.light, "one-light");
  assert.equal(next.custom.length, 0);
});
test("contrast warning uses measurable colour contrast", () => {
  assert.equal(contrastRatio("#ffffff", "#000000"), 21);
  assert.equal(contrastRatio("#777777", "#777777"), 1);
});
test("normalization filters duplicate and malformed custom IDs and bounds the palette library", () => {
  const p = { ...PALETTES[0], id: "custom-one" };
  const a = normalizeAppearance({
    ...defaultAppearance(),
    custom: [
      p,
      p,
      { ...p, id: "nord" },
      { ...p, id: "custom-bad", colors: { ...p.colors, bg: "red" } },
    ],
  });
  assert.equal(a.custom.length, 1);
  assert.equal(a.custom[0].id, "custom-one");
  assert.equal(
    normalizeAppearance({
      ...a,
      custom: Array.from({ length: 40 }, (_, i) => ({
        ...p,
        id: `custom-${i}`,
      })),
    }).custom.length,
    32,
  );
});
test("all font roles are independently bounded and headings retain discrete hierarchy scales", () => {
  const a = defaultAppearance();
  a.fonts.reading.size = 24;
  a.fonts.code.size = 11;
  a.fonts.heading = { family: "Songti SC", scale: 1.15 };
  assert.deepEqual(normalizeAppearance(a).fonts, a.fonts);
  assert.equal(
    normalizeAppearance({
      ...a,
      fonts: { ...a.fonts, heading: { family: "serif", scale: 30 } },
    }).fonts.heading.scale,
    1,
  );
});
test("missing colour keys, invalid scheme, empty names, and external font CSS are rejected", () => {
  for (const change of [
    { name: "" },
    { scheme: "system" },
    { colors: {} },
    { colors: { ...PALETTES[0].colors, bg: "#fff" } },
  ])
    assert.throws(() =>
      importPalette(
        JSON.stringify({ version: 1, palette: { ...PALETTES[0], ...change } }),
      ),
    );
  for (const family of [
    '"evil"',
    "Arial, url(x)",
    "var(--secret)",
    "x\ncolor:red",
    "font/remote",
  ])
    assert.ok(!fontStack(family, "reading").includes(family));
});
