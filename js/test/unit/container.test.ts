import { describe, expect, it, vi, afterEach } from "vitest";
import { AltEngine, type Job } from "../../src/index.js";

// The container client is thin over REST except for `wait`. What is worth asserting is what a
// caller would otherwise get wrong: that a retried launch is a second machine, that giving up
// waiting does not stop one, and that a page of jobs is a page.

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

function client(fetchImpl: typeof fetch) {
  return new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: fetchImpl });
}

const job = (over: Partial<Job> = {}): Job =>
  ({
    id: "j1",
    status: "running",
    image: "alpine:3",
    size: "small",
    exit_code: null,
    timeout_ms: 60_000,
    error: null,
    started_at: 1,
    ended_at: null,
    cost_usd: 0,
    ...over,
  }) as Job;

afterEach(() => {
  vi.useRealTimers();
});

describe("launching a job", () => {
  it("posts to the instance and returns the job", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      expect(String(url)).toBe("http://test.local/v1/container/jobs");
      expect(init.method).toBe("POST");
      expect(JSON.parse(init.body)).toEqual({ image: "alpine:3", cmd: ["sh", "-c", "echo hi"] });
      return json(201, { job: job() });
    });
    const out = await client(fetchMock as never).container("jobs").run({ image: "alpine:3", cmd: ["sh", "-c", "echo hi"] });
    expect(out.id).toBe("j1");
  });

  it("never retries a launch", async () => {
    // A retried launch is a SECOND MACHINE, billed for its own runtime.
    let calls = 0;
    const fetchMock = vi.fn(async () => {
      calls++;
      return json(503, { error: { code: "UNAVAILABLE", message: "try later" } });
    });
    await client(fetchMock as never).container("jobs").run({ image: "alpine:3" }).catch(() => {});
    expect(calls).toBe(1);
  });

  it("surfaces the refusal of an end-user token rather than retrying it", async () => {
    // There is no rule language for what a user may SPEND, so this service takes an org key
    // only. A page that needs a job calls a function that decides.
    const fetchMock = vi.fn(async () =>
      json(403, { error: { code: "FORBIDDEN", message: "container jobs require an organization API key, not an end-user token" } })
    );
    await expect(client(fetchMock as never).container("jobs").run({ image: "alpine:3" })).rejects.toThrow(
      /organization API key/
    );
  });
});

describe("waiting for a job", () => {
  it("polls until the job reaches a terminal status", async () => {
    vi.useFakeTimers();
    const statuses = ["running", "running", "done"];
    let i = 0;
    const fetchMock = vi.fn(async () =>
      json(200, { job: job({ status: statuses[Math.min(i++, 2)] as Job["status"], exit_code: 0 }) })
    );
    const p = client(fetchMock as never).container("jobs").wait("j1");
    await vi.runAllTimersAsync();
    expect((await p).status).toBe("done");
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("returns rather than hangs on a job that failed", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => json(200, { job: job({ status: "failed", exit_code: 1, error: "out of memory" }) }));
    const p = client(fetchMock as never).container("jobs").wait("j1");
    await vi.runAllTimersAsync();
    expect((await p).error).toBe("out of memory");
  });

  it("gives up on the WAIT without stopping the machine, and says so", async () => {
    // A caller who reads this as "the job stopped" leaves a machine running and billing.
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => json(200, { job: job() }));
    const p = client(fetchMock as never).container("jobs").wait("j1", { timeoutMs: 5_000 }).catch((e) => e);
    await vi.runAllTimersAsync();
    const err = await p;
    expect(String(err.message)).toContain("still billing");
    expect(String(err.message)).toContain("cancel it");
  });

  it("stops waiting when the caller aborts", async () => {
    const fetchMock = vi.fn(async () => json(200, { job: job() }));
    const ac = new AbortController();
    const p = client(fetchMock as never).container("jobs").wait("j1", { signal: ac.signal }).catch((e) => e);
    ac.abort(new Error("caller went away"));
    expect(String((await p).message)).toBe("caller went away");
  });

  it("launches and waits in one call", async () => {
    vi.useFakeTimers();
    let launched = false;
    const fetchMock = vi.fn(async (_u: any, init: any) => {
      if (init?.method === "POST") {
        launched = true;
        return json(201, { job: job() });
      }
      return json(200, { job: job({ status: "done", exit_code: 0 }) });
    });
    const p = client(fetchMock as never).container("jobs").runAndWait({ image: "alpine:3" });
    await vi.runAllTimersAsync();
    expect((await p).exit_code).toBe(0);
    expect(launched).toBe(true);
  });
});

describe("listing jobs", () => {
  it("hands back the cursor rather than pretending the page is every job", async () => {
    const fetchMock = vi.fn(async () => json(200, { jobs: [job()], cursor: 1700 }));
    const page = await client(fetchMock as never).container("jobs").list({ status: "done" });
    expect(page.cursor).toBe(1700);
  });

  it("feeds the cursor back as `before`, which is what this endpoint calls it", async () => {
    const pages = [
      { jobs: [job({ id: "a" })], cursor: 1700 },
      { jobs: [job({ id: "b" })], cursor: null },
    ];
    let n = 0;
    const seen: string[] = [];
    const fetchMock = vi.fn(async (url: any) => {
      seen.push(String(url));
      return json(200, pages[n++]);
    });
    const ids: string[] = [];
    for await (const j of client(fetchMock as never).container("jobs").listAll({ status: "done" })) ids.push(j.id);
    expect(ids).toEqual(["a", "b"]);
    expect(seen[1]).toContain("before=1700");
    expect(seen.every((u) => u.includes("status=done"))).toBe(true);
  });
});

describe("logs", () => {
  it("returns an empty page with a note rather than throwing when the log service is down", async () => {
    // A job's own record — exit code, timing, cost — never depends on the log service, so a
    // detail view must not become a 500 because the logs could not be read.
    const fetchMock = vi.fn(async (url: any) => {
      expect(String(url)).toContain("/v1/container/jobs/j1/logs");
      return json(200, { lines: [], cursor: null, note: "logs for this job are no longer available" });
    });
    const page = await client(fetchMock as never).container("jobs").logs("j1");
    expect(page.lines).toEqual([]);
    expect(page.note).toContain("no longer available");
  });
});

describe("cancelling", () => {
  it("reports a job that had already finished as not cancelled, which is not a failure", async () => {
    const fetchMock = vi.fn(async () => json(200, { job: job({ status: "done" }), canceled: false }));
    const out = await client(fetchMock as never).container("jobs").cancel("j1");
    expect(out.canceled).toBe(false);
    expect(out.job.status).toBe("done");
  });
});
