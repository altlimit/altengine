import { beforeAll, describe, expect, it } from "vitest";
import { AltEngineError, type ContainerClient } from "../../src/index.js";
import { client, uniq } from "./helpers.js";

// Locally, jobs run on the caller's own Docker daemon — the one service here that needs
// something the emulator cannot provide. So this suite asserts everything that does not need a
// container to exist, and asserts that a launch without Docker REFUSES rather than pretending a
// job ran.

const instance = uniq("sdk-conf");
let jobs: ContainerClient;

beforeAll(() => {
  jobs = client().container(instance);
});

describe("what an instance will accept", () => {
  it("reports its sizes, its image allowlist and its ceilings", async () => {
    const s = await jobs.sizes();
    expect(s.sizes.map((x) => x.name)).toContain("small");
    for (const size of s.sizes) {
      expect(size.cpus).toBeGreaterThan(0);
      expect(size.memory_mb).toBeGreaterThan(0);
    }
    // An empty allowlist runs nothing — it is the default, not a misconfiguration.
    expect(Array.isArray(s.allowed_images)).toBe(true);
    expect(s.max_timeout_ms).toBeGreaterThan(0);
    expect(s.max_concurrent).toBeGreaterThan(0);
  });
});

describe("jobs", () => {
  it("lists an empty instance as empty, with nothing older to fetch", async () => {
    const page = await jobs.list();
    expect(page.jobs).toEqual([]);
    expect(page.cursor).toBeNull();
  });

  it("does not find a job that does not exist", async () => {
    await expect(jobs.get("jdoesnotexist")).rejects.toThrow(/not found/i);
  });

  it("refuses a launch it cannot honour instead of reporting one it did not run", async () => {
    // Two honest refusals, and which one you get says what is missing: no Docker daemon, or an
    // image the instance was never told to allow.
    const err = await jobs.run({ image: "alpine:3", cmd: ["sh", "-c", "echo hi"] }).catch((e) => e);
    expect(err).toBeInstanceOf(AltEngineError);
    expect([403, 503]).toContain((err as AltEngineError).status);
    expect(String((err as AltEngineError).message)).toMatch(/Docker|allowed images/);
  });
});
