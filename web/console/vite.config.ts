import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import path from "path";
import { build, defineConfig, type Plugin } from "vite";

// Build the shared appearance bootstrap once, inline, so preferences are
// available before CSS paints (not a separate, drifting storage parser).
function appearanceBoot(): Plugin {
  let script: Promise<string> | undefined;
  return {
    name: "appearance-boot",
    transformIndexHtml: {
      order: "pre",
      async handler(html) {
        if (!html.includes("<!-- appearance-boot -->")) return html;
        script ??= (async () => {
          const result = await build({
            configFile: false,
            logLevel: "error",
            build: {
              write: false,
              minify: true,
              lib: {
                entry: path.resolve(__dirname, "src/lib/appearance-boot.ts"),
                name: "SteveAppearanceBoot",
                formats: ["iife"],
              },
            },
          });
          const outputs = Array.isArray(result) ? result : [result];
          const output = outputs
            .flatMap((value) => ("output" in value ? value.output : []))
            .find((value) => value.type === "chunk");
          if (!output || output.type !== "chunk")
            throw new Error("Appearance bootstrap was not built");
          return output.code.replace(/<\/script/gi, "<\\/script");
        })();
        return html.replace(
          "<!-- appearance-boot -->",
          `<script>${await script}</script>`,
        );
      },
    },
  };
}

// The build lands inside the Go module and is embedded into the binary:
// the hub serves the console itself, with no asset directory at runtime.
export default defineConfig({
  plugins: [appearanceBoot(), react(), tailwindcss()],
  base: "./",
  resolve: { alias: { "@": path.resolve(__dirname, "./src") } },
  build: {
    outDir: "../../internal/readmodel/web/dist",
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 1200,
  },
});
