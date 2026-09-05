import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import path from "path";
import { defineConfig } from "vite";

// The build lands inside the Go module and is embedded into the binary:
// the hub serves the console itself, with no asset directory at runtime.
export default defineConfig({
    plugins: [react(), tailwindcss()],
    base: "./",
    resolve: { alias: { "@": path.resolve(__dirname, "./src") } },
    build: { outDir: "../../internal/readmodel/web/dist", emptyOutDir: true, sourcemap: false, chunkSizeWarningLimit: 1200 },
});
