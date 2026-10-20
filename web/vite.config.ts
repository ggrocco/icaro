import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The daemon serves the built UI under /ui/ and the API under /api/.
export default defineConfig({
  base: "/ui/",
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
  server: {
    proxy: { "/api": "http://127.0.0.1:8787", "/healthz": "http://127.0.0.1:8787" },
  },
  test: { environment: "node", include: ["src/**/*.test.ts"] },
});
