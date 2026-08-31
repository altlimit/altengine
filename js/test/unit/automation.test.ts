import { describe, expect, it, vi, afterEach } from "vitest";
import { AltEngine } from "../../src/index.js";
import type { Run } from "../../src/index.js";

// The automation client is thin over REST except for `wait`, which is where the behaviour lives:
// polling a job that drives a real desktop for minutes or hours. What is worth asserting is what
// a caller would otherwise get wrong — that a queued run is not a failure, that a retried start
// would be a second pass over the same invoices, and that giving up waiting does not stop the run.

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

function client(fetchImpl: typeof fetch) {
  return new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: fetchImpl });
}

const run = (over: Partial<Run> = {}): Run =>
  ({
    id: "r1",
    status: "running",
    script: "nightly",
    version: 3,
    agent_id: "ai1.a",
    params: null,
    labels: [],
    parallel: false,
    timeout_ms: 0,
    max_run_seconds: 14000,
    parent_run_id: null,
    chain_depth: 0,
    exit_code: null,
    error: null,
    cost_usd: 0,
    queued_at: 1,
    started_at: 2,
    ended_at: null,
    webhook: null,
    ...over,
  }) as Run;

afterEach(() => {
  vi.useRealTimers();
});

describe("starting a run", () => {
  it("posts to the instance's runs endpoint and returns the run", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      expect(String(url)).toBe("http://test.local/v1/automation/fleet/runs");
      expect(init.method).toBe("POST");
      expect(JSON.parse(init.body)).toEqual({ script: "nightly", params: { date: "2026-08-30" } });
      return json(201, { run: run() });
    });
    const a = client(fetchMock as any).automation("fleet");
    expect((await a.start({ script: "nightly", params: { date: "2026-08-30" } })).id).toBe("r1");
  });

  it("treats a queued run as a run, not a failure", async () => {
    // 202, because no machine matching the request is online yet — and an office PC being asleep
    // is the ordinary case, not an error the caller should throw on.
    const fetchMock = vi.fn(async () => json(202, { run: run({ status: "queued", started_at: null }) }));
    const a = client(fetchMock as any).automation("fleet");
    expect((await a.start({ script: "nightly" })).status).toBe("queued");
  });

  it("never retries a start", async () => {
    // A retried start is a SECOND RUN. It drives a real application, so the duplicate is another
    // pass over the same invoices rather than a wasted request.
    let calls = 0;
    const fetchMock = vi.fn(async () => {
      calls++;
      return json(503, { error: { code: "UNAVAILABLE", message: "try later" } });
    });
    const a = client(fetchMock as any).automation("fleet");
    await a.start({ script: "nightly" }).catch(() => {});
    expect(calls).toBe(1);
  });
});

describe("waiting for a run", () => {
  it("polls until the run reaches a terminal status, then returns its artifacts", async () => {
    vi.useFakeTimers();
    const statuses = ["running", "running", "done"];
    let i = 0;
    const fetchMock = vi.fn(async () =>
      json(200, {
        run: run({ status: statuses[Math.min(i++, statuses.length - 1)] as Run["status"] }),
        artifacts: [{ id: "a1", run_id: "r1", name: "export.csv", size_bytes: 10, url: "https://x/1" }],
      })
    );
    const a = client(fetchMock as any).automation("fleet");

    const p = a.wait("r1");
    await vi.runAllTimersAsync();
    const got = await p;
    expect(got.run.status).toBe("done");
    // A `done` run always has its output: the agent holds the terminal report until every
    // artifact has landed, so the links here are live rather than hopeful.
    expect(got.artifacts[0]?.url).toBe("https://x/1");
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("returns rather than hangs on a run that failed", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () =>
      json(200, { run: run({ status: "failed", error: "no window matching \"QuickBooks\" appeared" }), artifacts: [] })
    );
    const a = client(fetchMock as any).automation("fleet");
    const p = a.wait("r1");
    await vi.runAllTimersAsync();
    const got = await p;
    expect(got.run.status).toBe("failed");
    expect(got.run.error).toContain("QuickBooks");
  });

  it("gives up on the WAIT without stopping the run, and says so", async () => {
    // The distinction matters: a caller who reads this as "the run stopped" leaves a job driving
    // somebody's desktop and never looks again.
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => json(200, { run: run({ status: "running" }), artifacts: [] }));
    const a = client(fetchMock as any).automation("fleet");
    const p = a.wait("r1", { timeoutMs: 5_000 }).catch((e) => e);
    await vi.runAllTimersAsync();
    const err = await p;
    expect(String(err.message)).toContain("still going on the machine");
    expect(String(err.message)).toContain("cancel it");
  });

  it("stops waiting when the caller aborts", async () => {
    // `wait` inside a request handler has to die with the request rather than holding it open
    // until a run nobody is waiting for any more finishes.
    const fetchMock = vi.fn(async () => json(200, { run: run({ status: "running" }), artifacts: [] }));
    const a = client(fetchMock as any).automation("fleet");
    const ac = new AbortController();
    const p = a.wait("r1", { signal: ac.signal }).catch((e) => e);
    ac.abort(new Error("caller went away"));
    expect(String((await p).message)).toBe("caller went away");
  });

  it("reports each poll, so a caller can show what it is doing", async () => {
    vi.useFakeTimers();
    const seen: (string | null | undefined)[] = [];
    const phases = [
      { status: "running", phase: null },
      { status: "running", phase: "uploading" },
      { status: "done", phase: null },
    ];
    let i = 0;
    const fetchMock = vi.fn(async () => {
      const p = phases[Math.min(i++, phases.length - 1)];
      return json(200, { run: run(p as Partial<Run>), artifacts: [] });
    });
    const a = client(fetchMock as any).automation("fleet");
    const p = a.wait("r1", { onPoll: (r) => seen.push(r.phase) });
    await vi.runAllTimersAsync();
    await p;
    // The middle poll is the one that matters: it says a 3 GB export is going up, not that the
    // script is stuck.
    expect(seen).toEqual([null, "uploading", null]);
  });
});

// Inbound data is the one path that goes toward the agent. What is worth asserting is the two
// things a caller gets wrong: that it is addressed by KEY rather than by run id, and that
// "nothing was running" is an error rather than a quiet success.
describe("sending a value to a waiting job", () => {
  it("addresses it by key, and passes the narrowing filters as query", async () => {
    const fetchImpl = vi.fn(async (url: string | URL) => {
      const u = new URL(String(url));
      expect(u.pathname).toBe("/v1/automation/fleet/data/otp.dealer-42");
      expect(u.searchParams.get("script")).toBe("portal-login");
      expect(u.searchParams.get("agent_id")).toBe("ai1.a");
      return json(200, { key: "otp.dealer-42", delivered: 1, runs: ["r1"], unreachable: [] });
    }) as unknown as typeof fetch;

    const out = await client(fetchImpl)
      .automation("fleet")
      .send("otp.dealer-42", "123456", { script: "portal-login", agentId: "ai1.a" });
    expect(out.delivered).toBe(1);
  });

  it("sends the value as the whole body, not wrapped in an envelope", async () => {
    // A poster is usually a webhook handler forwarding what it parsed. Wrapping it would mean
    // every script unwrapping it again at the far end, where the code is hardest to change.
    let body: string | undefined;
    const fetchImpl = vi.fn(async (_u: string | URL, init?: RequestInit) => {
      body = init?.body as string;
      return json(200, { delivered: 1, runs: ["r1"], unreachable: [] });
    }) as unknown as typeof fetch;

    await client(fetchImpl).automation("fleet").send("otp", { code: "123456" });
    expect(JSON.parse(body!)).toEqual({ code: "123456" });
  });

  it("throws when no run is executing, rather than reporting a quiet success", async () => {
    // The overwhelmingly common failure is a race the poster cannot see: the code arrived four
    // seconds after the script gave up. An integration has to be able to log or retry that.
    const fetchImpl = vi.fn(async () =>
      json(409, { error: { code: "FAILED_PRECONDITION", message: "no run is executing on this instance right now" } })
    ) as unknown as typeof fetch;

    await expect(client(fetchImpl).automation("fleet").send("otp", "1")).rejects.toThrow(/no run is executing/);
  });

  it("does not retry a delivery", async () => {
    // A one-time code delivered twice is at best noise and at worst a second login attempt
    // against a portal that counts them.
    const fetchImpl = vi.fn(async () => json(500, { error: { message: "boom" } })) as unknown as typeof fetch;
    await expect(client(fetchImpl).automation("fleet").send("otp", "1")).rejects.toThrow();
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  it("can name one run when it has the id", async () => {
    const fetchImpl = vi.fn(async (url: string | URL) => {
      expect(new URL(String(url)).pathname).toBe("/v1/automation/fleet/runs/r1/data/otp");
      return json(200, { delivered: true });
    }) as unknown as typeof fetch;
    await client(fetchImpl).automation("fleet").sendToRun("r1", "otp", "1");
  });
});

describe("credentials", () => {
  it("reads names, never values", async () => {
    const fetchImpl = vi.fn(async () => json(200, { env: ["PORTAL_PASSWORD", "PORTAL_USER"] })) as unknown as typeof fetch;
    expect(await client(fetchImpl).automation("fleet").env()).toEqual(["PORTAL_PASSWORD", "PORTAL_USER"]);
  });

  it("sends null to keep a stored value, which is the only way to rotate one of several", async () => {
    let body: any;
    const fetchImpl = vi.fn(async (_u: string | URL, init?: RequestInit) => {
      body = JSON.parse(init?.body as string);
      return json(200, { env: ["PORTAL_PASSWORD", "PORTAL_USER"] });
    }) as unknown as typeof fetch;

    await client(fetchImpl).automation("fleet").setEnv({ PORTAL_PASSWORD: "new", PORTAL_USER: null });
    expect(body.env).toEqual({ PORTAL_PASSWORD: "new", PORTAL_USER: null });
  });
});
