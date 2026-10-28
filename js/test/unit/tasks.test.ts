import { describe, expect, it, vi } from "vitest";
import { AltEngine } from "../../src/index.js";

// The tasks client is thin over REST. What is worth asserting is what a caller would get wrong by
// hand: a taken name is a result rather than a throw, an unnamed add is never retried, a Date
// becomes epoch ms, and the paths and queries are the documented ones.

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

function client(fetchImpl: typeof fetch) {
  return new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: fetchImpl }).tasks("jobs");
}

describe("adding", () => {
  it("sends { tasks } with a Date as epoch ms", async () => {
    const when = new Date("2027-01-15T10:00:00Z");
    const fetchMock = vi.fn(async (url: any, init: any) => {
      expect(String(url)).toBe("http://test.local/v1/tasks/jobs/queues/emails/tasks");
      expect(init.method).toBe("POST");
      expect(JSON.parse(init.body)).toEqual({
        tasks: [{ payload: { to: "a@example.com" }, name: "welcome-1", runAt: when.getTime(), params: { kind: "welcome" } }],
      });
      return json(201, { tasks: [{ ok: true, id: "t1", name: "welcome-1", state: "scheduled", runAt: when.getTime() }] });
    });
    const res = await client(fetchMock as never).add("emails", {
      payload: { to: "a@example.com" },
      name: "welcome-1",
      runAt: when,
      params: { kind: "welcome" },
    });
    expect(res).toEqual([{ ok: true, id: "t1", name: "welcome-1", state: "scheduled", runAt: when.getTime() }]);
  });

  it("returns the results when every name was taken (409), not a throw", async () => {
    const refused = { ok: false, code: "TASK_EXISTS", message: "a task named 'a' already exists in queue 'q'", id: "t0", name: "a" };
    const fetchMock = vi.fn(async () => json(409, { tasks: [refused], error: { code: "TASK_EXISTS", message: refused.message } }));
    expect(await client(fetchMock as never).add("q", [{ name: "a", payload: null }])).toEqual([refused]);
  });

  it("never retries an unnamed add, and retries a named one", async () => {
    let calls = 0;
    const flaky = vi.fn(async () => {
      calls++;
      return json(503, { error: { code: "UNAVAILABLE", message: "try later" } });
    });
    const c = new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: flaky as never, retry: { maxAttempts: 3, baseDelayMs: 1, maxDelayMs: 1 } }).tasks("jobs");
    await expect(c.add("q", { payload: 1 })).rejects.toThrow(/try later/);
    expect(calls).toBe(1);
    calls = 0;
    await expect(c.add("q", { name: "n", payload: 1 })).rejects.toThrow(/try later/);
    expect(calls).toBe(3);
  });

  it("still throws a real refusal", async () => {
    const fetchMock = vi.fn(async () => json(400, { error: { code: "NO_TARGET", message: "queue 'default' has no target" } }));
    await expect(client(fetchMock as never).add("default", { payload: 1 })).rejects.toMatchObject({ code: "NO_TARGET", status: 400 });
  });
});

describe("queues and tasks", () => {
  it("creates with the name in the body and patches by path", async () => {
    const seen: string[] = [];
    const fetchMock = vi.fn(async (url: any, init: any) => {
      seen.push(`${init.method} ${new URL(String(url)).pathname} ${init.body ?? ""}`);
      return json(init.method === "POST" ? 201 : 200, { queue: { name: "emails" } });
    });
    const c = client(fetchMock as never);
    await c.createQueue("emails", { target: { function: "app/send" }, ratePerSecond: 2 });
    await c.updateQueue("emails", { paused: true });
    await c.pauseQueue("emails");
    expect(seen).toEqual([
      `POST /v1/tasks/jobs/queues {"target":{"function":"app/send"},"ratePerSecond":2,"name":"emails"}`,
      `PATCH /v1/tasks/jobs/queues/emails {"paused":true}`,
      `POST /v1/tasks/jobs/queues/emails/pause `,
    ]);
  });

  it("lists with the documented query and pages through listAll", async () => {
    const fetchMock = vi.fn(async (url: any) => {
      const u = new URL(String(url));
      expect(u.pathname).toBe("/v1/tasks/jobs/queues/q/tasks");
      expect(u.searchParams.get("state")).toBe("failed");
      return u.searchParams.get("cursor")
        ? json(200, { tasks: [{ id: "b" }], cursor: null })
        : json(200, { tasks: [{ id: "a" }], cursor: "1.a" });
    });
    const ids: string[] = [];
    for await (const t of client(fetchMock as never).listAll("q", { state: "failed" })) ids.push(t.id);
    expect(ids).toEqual(["a", "b"]);
  });

  it("cancels one id or many through one call", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      expect(String(url)).toBe("http://test.local/v1/tasks/jobs/tasks/cancel");
      expect(JSON.parse(init.body)).toEqual({ ids: ["t1"] });
      return json(200, { cancelled: ["t1"], skipped: [] });
    });
    expect((await client(fetchMock as never).cancel("t1")).cancelled).toEqual(["t1"]);
  });

  it("leases without retrying and acks with the lease id", async () => {
    let leases = 0;
    const fetchMock = vi.fn(async (url: any, init: any) => {
      const path = new URL(String(url)).pathname;
      if (path.endsWith("/lease")) {
        leases++;
        return json(503, { error: { code: "UNAVAILABLE", message: "busy" } });
      }
      expect(path).toBe("/v1/tasks/jobs/tasks/t1/ack");
      expect(JSON.parse(init.body)).toEqual({ leaseId: "L", result: { ok: 1 } });
      return json(200, { task: { id: "t1", state: "done" } });
    });
    const c = new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: fetchMock as never, retry: { maxAttempts: 3, baseDelayMs: 1, maxDelayMs: 1 } }).tasks("jobs");
    await expect(c.lease("work", { count: 5 })).rejects.toThrow(/busy/);
    expect(leases).toBe(1);
    expect((await c.ack("t1", "L", { ok: 1 })).state).toBe("done");
  });
});
