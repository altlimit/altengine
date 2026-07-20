import { defineConfig } from "tsup";

export default defineConfig({
  entry: {
    index: "src/index.ts",
    // Browser-safe subpath: the channel WebSocket client only (no API-key code).
    channel: "src/channel/socket.ts",
    // Browser-safe subpath: end-user sign-in. Also API-key-free — it ships to a
    // browser, where an org key never belongs.
    auth: "src/auth/client.ts",
  },
  format: ["esm", "cjs"],
  dts: true,
  sourcemap: true,
  clean: true,
  target: "es2022",
});
