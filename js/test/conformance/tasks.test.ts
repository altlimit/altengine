import { beforeAll, describe, expect, it } from "vitest";
import { AltEngineError, type TasksClient } from "../../src/index.js";
import { client, destructiveOk, uniq } from "./helpers.js";

// A pull queue needs nothing but the tasks service, so the whole lease/ack cycle runs against the
// emulator and a hosted instance alike. Push delivery needs a deployed function and is covered by
// the emulator's own suite.

const instance = uniq("sdk-conf");
let tq: TasksClient;

beforeAll(() => {
  tq = client().tasks(instance);
});

describe("queues", () => {
  it("has a default queue and creates, patches, pauses and deletes others", async () => {
    if (!destructiveOk()) return;
    const info = await tq.info();
    expect(info.queues.map((q) => q.name)).toContain("default");
    expect(info.retentionDays).toBeGreaterThan(0);

    const q = await tq.createQueue("work", { mode: "pull", retry: { maxAttempts: 2 } });
    expect(q.settings.mode).toBe("pull");
    expect(q.settings.target).toBeNull();
    expect(q.settings.retry.maxAttempts).toBe(2);
    expect(q.settings.deadlineSeconds).toBe(600);
    // Only what is sent changes.
    const patched = await tq.updateQueue("work", { maxConcurrent: 3, deadlineSeconds: 120 });
    expect(patched.settings.mode).toBe("pull");
    expect(patched.settings.maxConcurrent).toBe(3);
    expect(patched.settings.deadlineSeconds).toBe(120);
    const short = await tq.updateQueue("work", { deadlineSeconds: 5 }).catch((e) => e);
    expect(short.code).toBe("INVALID_ARGUMENT");
    expect((await tq.pauseQueue("work")).settings.paused).toBe(true);
    expect((await tq.resumeQueue("work")).settings.paused).toBe(false);

    const err = await tq.createQueue("work").catch((e) => e);
    expect(err).toBeInstanceOf(AltEngineError);
    expect(err.code).toBe("ALREADY_EXISTS");

    const bad = await tq.deleteQueue("default").catch((e) => e);
    expect(bad).toBeInstanceOf(AltEngineError);
    expect(bad.status).toBe(400);
  });
});

describe("a pull queue", () => {
  it("adds idempotently by name, leases, acks and releases", async () => {
    if (!destructiveOk()) return;
    const added = await tq.add("work", [
      { name: "job-1", payload: { n: 1 } },
      { payload: { n: 2 }, params: { via: "conformance" }, deadlineSeconds: 45 },
    ]);
    expect(added.map((r) => r.ok)).toEqual([true, true]);
    const again = await tq.add("work", { name: "job-1", payload: { n: 1 } });
    expect(again[0]).toMatchObject({ ok: false, code: "TASK_EXISTS", name: "job-1" });

    const leased = await tq.lease<{ n: number }>("work", { count: 10, leaseSeconds: 60 });
    expect(leased).toHaveLength(2);
    expect(leased[0]!.payload.n).toBe(1);
    expect(leased[0]!.state).toBe("leased");
    expect(leased[0]!.deadlineSeconds).toBeNull();
    expect(leased[1]!.deadlineSeconds).toBe(45);

    const done = await tq.ack(leased[0]!.id, leased[0]!.leaseId, { handled: true });
    expect(done.state).toBe("done");
    expect(done.result).toBe('{"handled":true}');

    const lost = await tq.ack(leased[1]!.id, "not-the-lease").catch((e) => e);
    expect(lost.code).toBe("LEASE_LOST");

    const back = await tq.release(leased[1]!.id, leased[1]!.leaseId, { error: "later", delaySeconds: 600 });
    expect(back.state).toBe("scheduled");
    expect(back.lastError).toBe("later");
    expect(back.params).toEqual({ via: "conformance" });

    const page = await tq.list("work", { state: "done" });
    expect(page.tasks.map((t) => t.id)).toEqual([leased[0]!.id]);
    const one = await tq.get(leased[0]!.id);
    expect(one.attemptLog[0]!.status).toBe("acked");

    const cancelled = await tq.cancel([leased[1]!.id, leased[0]!.id]);
    expect(cancelled.cancelled).toEqual([leased[1]!.id]);
    expect(cancelled.skipped).toEqual([{ id: leased[0]!.id, state: "done" }]);

    const purged = await tq.purgeQueue("work");
    expect(purged.purged).toBe(2);
    expect((await tq.deleteQueue("work")).deleted).toBe(true);
  });
});
