import { writeFile, readFile } from "node:fs/promises";
import { PALETTES } from "../src/lib/themes.ts";
const output = new URL("../src/styles/palette-seeds.css", import.meta.url);
const css =
  "/* Generated from lib/themes.ts by scripts/generate-palettes.mjs. */\n" +
  PALETTES.map(
    (p) =>
      `:root[data-theme="${p.id}"], [data-appearance-preview][data-theme="${p.id}"] {\n${Object.entries(
        p.colors,
      )
        .map(([k, v]) => `    --seed-${k}: ${v};`)
        .join("\n")}\n}\n`,
  ).join("\n");
if (process.argv.includes("--check")) {
  if ((await readFile(output, "utf8")) !== css)
    throw new Error("Palette CSS is stale; run npm run generate:palettes");
} else await writeFile(output, css);
