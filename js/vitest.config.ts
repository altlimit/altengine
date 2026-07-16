import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    include: ["test/**/*.test.ts"],
    // The conformance suite spawns the emulator once for the whole run.
    globalSetup: ["test/conformance/setup.ts"],
    testTimeout: 20_000,
    hookTimeout: 60_000,
  },
});
