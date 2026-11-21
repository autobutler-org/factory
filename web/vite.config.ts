/// <reference types="vitest/config" />
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// One config for the dev server, the build and Vitest. The build goes where the Go server embeds
// it (internal/server); `make build/web` empties that directory first, keeping its stub.txt.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/server/public",
    emptyOutDir: false,
  },
  // `make serve/web`: /api goes to the `factory serve` at FACTORY_URL (default: its default address).
  server: {
    proxy: { "/api": process.env.FACTORY_URL ?? "http://127.0.0.1:8080" },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./vitest.setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
