/** vitest globalSetup: build the emulator once and spawn it on a free port; the
 * returned function tears it down. When ALTENGINE_CONFORMANCE_URL is set, the suite
 * targets that instead and no emulator is spawned. */
import { spawn, execFileSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import { createServer } from "node:net";
import path from "node:path";

const freePort = (): Promise<number> =>
  new Promise((resolve, reject) => {
    const srv = createServer();
    srv.listen(0, "127.0.0.1", () => {
      const port = (srv.address() as { port: number }).port;
      srv.close(() => resolve(port));
    });
    srv.on("error", reject);
  });

export default async function setup(): Promise<() => void> {
  if (process.env.ALTENGINE_CONFORMANCE_URL) {
    process.env.ALTENGINE_TEST_URL = process.env.ALTENGINE_CONFORMANCE_URL;
    process.env.ALTENGINE_TEST_KEY = process.env.ALTENGINE_CONFORMANCE_KEY ?? "";
    return () => {};
  }

  const jsDir = path.resolve(__dirname, "../..");
  const repoRoot = path.resolve(jsDir, "..");
  const binDir = path.join(jsDir, "test", ".bin");
  const bin = path.join(binDir, process.platform === "win32" ? "altengine.exe" : "altengine");
  mkdirSync(binDir, { recursive: true });
  execFileSync("go", ["build", "-o", bin, "./cmd/altengine"], { cwd: path.join(repoRoot, "cli"), stdio: "inherit" });

  const port = await freePort();
  // stdio "ignore": an inherited pipe would keep the test runner's output stream
  // open past exit if teardown ever misses the kill.
  const child = spawn(bin, ["dev", "--memory", "--port", String(port)], { stdio: "ignore" });

  const base = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + 15_000;
  for (;;) {
    try {
      const res = await fetch(`${base}/healthz`);
      if (res.ok) break;
    } catch {
      // not up yet
    }
    if (Date.now() > deadline) {
      child.kill("SIGKILL");
      throw new Error("emulator did not become healthy in 15s");
    }
    await new Promise((r) => setTimeout(r, 100));
  }

  process.env.ALTENGINE_TEST_URL = base;
  process.env.ALTENGINE_TEST_KEY = "conformance-dev-key";

  return () => {
    child.kill("SIGTERM");
  };
}
