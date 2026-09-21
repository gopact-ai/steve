import { defaultAppearance, readAppearance } from "./appearance.ts";
import { applyAppearance } from "./appearance-dom.ts";
// Bundled inline in the head; uses exactly the same validation and renderer
// as React, including custom palettes and fonts, before the first paint.
try {
  applyAppearance(
    readAppearance(localStorage),
    matchMedia("(prefers-color-scheme: dark)").matches,
  );
} catch {
  applyAppearance(
    defaultAppearance(),
    matchMedia("(prefers-color-scheme: dark)").matches,
  );
}
