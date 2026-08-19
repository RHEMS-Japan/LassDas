import { defineConfig } from "vite";

export default defineConfig({
  esbuild: { jsx: "automatic" },
  build: { outDir: "../dist", emptyOutDir: true },
  server: { proxy: { "/api": "http://127.0.0.1:8542" } },
});
