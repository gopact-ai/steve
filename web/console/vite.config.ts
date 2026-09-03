import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build lands inside the Go module and is embedded into the binary:
// the hub serves the console itself, with no asset directory at runtime.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: { outDir: "../../internal/readmodel/web/dist", emptyOutDir: true, sourcemap: false },
});
