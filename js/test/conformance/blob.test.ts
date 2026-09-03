import { beforeAll, describe, expect, it } from "vitest";
import type { BlobClient } from "../../src/index.js";
import { client, destructiveOk, uniq } from "./helpers.js";

// Blob's whole shape is that the bytes do not travel through the API, so a suite that mocks the
// transfer proves nothing. Every upload here goes to a real presigned URL and comes back from
// one, against the emulator or a hosted instance.

const instance = uniq("sdk-conf");
let blob: BlobClient;

beforeAll(() => {
  blob = client().blob(instance);
});

describe("uploading and reading back", () => {
  it("round-trips bytes through a presigned URL", async () => {
    const rec = await blob.put("hello.txt", "hello world", { contentType: "text/plain" });
    expect(rec.status).toBe("ready");
    expect(rec.size).toBe(11);
    expect(rec.content_type).toBe("text/plain");
    // The portable handle: what you store in a document or hand to another service.
    expect(rec.blobkey).toMatch(/^blob:[^:]+:[^:]+$/);

    const bytes = await blob.bytes(rec.id);
    expect(new TextDecoder().decode(bytes)).toBe("hello world");

    const got = await blob.get(rec.id);
    expect(got.blob.id).toBe(rec.id);
    expect(got.download_url).toBeTruthy();
    expect(got.expires_at).toBeGreaterThan(Date.now());
  });

  it("stores metadata and reports the name it cleaned", async () => {
    const rec = await blob.put("report.json", JSON.stringify({ ok: true }), {
      contentType: "application/json",
      meta: { period: "2026-08" },
    });
    expect(rec.meta).toEqual({ period: "2026-08" });
    expect(rec.name).toBe("report.json");
  });

  it("refuses a reservation whose size the instance will not take", async () => {
    // The ceiling has to bite when the URL is minted; a URL signed for more is a URL that
    // stores more.
    await expect(blob.uploadUrl({ name: "huge.bin", size: 50 * 1024 ** 3 })).rejects.toThrow(/exceeds|limit/i);
  });

  it("mints an upload URL a client can use without an API key", async () => {
    const bytes = new TextEncoder().encode("direct");
    const minted = await blob.uploadUrl({ name: "direct.txt", size: bytes.length, content_type: "text/plain" });
    expect(minted.method).toBe("PUT");
    expect(minted.required_headers["content-length"]).toBe(String(bytes.length));

    const res = await fetch(minted.upload_url, {
      method: "PUT",
      headers: { "content-type": "text/plain" },
      body: bytes,
    });
    expect(res.ok).toBe(true);

    // No commit step: the row is promoted by the side that received the bytes.
    const got = await blob.get(minted.id);
    expect(got.blob.status).toBe("ready");
    expect(got.blob.size).toBe(bytes.length);
  });
});

describe("publishing", () => {
  it("gives a public object a stable URL that serves without a credential", async () => {
    const rec = await blob.put("logo.txt", "a logo, honest", { contentType: "text/plain", public: true });
    expect(rec.public).toBe(true);
    expect(rec.url).toBeTruthy();

    const res = await fetch(rec.url!);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("a logo, honest");

    const off = await blob.setPublic(rec.id, false);
    expect(off.public).toBe(false);
    expect(off.url).toBeNull();
  });
});

describe("listing", () => {
  it("filters by prefix and pages with a cursor", async () => {
    const prefix = uniq("batch");
    for (const n of [1, 2, 3]) await blob.put(`${prefix}-${n}.txt`, `n=${n}`, { contentType: "text/plain" });

    const page = await blob.list({ prefix, limit: 2 });
    expect(page.blobs).toHaveLength(2);
    expect(page.cursor).toBeTruthy();

    const seen: string[] = [];
    for await (const b of blob.listAll({ prefix })) seen.push(b.name);
    expect(seen.sort()).toEqual([`${prefix}-1.txt`, `${prefix}-2.txt`, `${prefix}-3.txt`]);
  });
});

describe("deleting", () => {
  it("removes objects and counts only the ones that existed", async () => {
    if (!destructiveOk()) return;
    const rec = await blob.put("gone.txt", "bye", { contentType: "text/plain" });
    const { deleted } = await blob.delete([rec.id, "not-a-real-id"]);
    expect(deleted).toBe(1);
    await expect(blob.get(rec.id)).rejects.toThrow(/not found/i);
  });

  it("refuses an empty delete rather than reporting a successful no-op", async () => {
    await expect(blob.delete([])).rejects.toThrow(/non-empty/i);
  });
});

describe("multipart", () => {
  it("cuts a file into parts and storage glues it back into one object", async () => {
    // Driven through the primitives rather than `put`, because the single-PUT ceiling is 5 GB
    // and a suite that actually allocated that would be a suite nobody runs.
    const body = new TextEncoder().encode("part one.part two.");
    const begun = await blob.beginMultipart({ name: "big.txt", size: body.length, content_type: "text/plain" });
    expect(begun.parts).toBeGreaterThanOrEqual(1);

    const created = await fetch(begun.create_url, { method: "POST", headers: { "content-type": "text/plain" } });
    expect(created.ok).toBe(true);
    const uploadId = /<UploadId>([^<]+)<\/UploadId>/.exec(await created.text())?.[1];
    expect(uploadId).toBeTruthy();

    const urls = await blob.multipartUrls({ id: begun.id, upload_id: uploadId!, from: 1, count: 1 });
    const part = urls.part_urls[0]!;
    const put = await fetch(part.url, { method: "PUT", body });
    expect(put.ok).toBe(true);
    // The digest storage computed. Without it the completion cannot name the part.
    const etag = put.headers.get("etag");
    expect(etag).toBeTruthy();

    const done = await fetch(urls.complete_url, {
      method: "POST",
      headers: { "content-type": "application/xml" },
      body: `<CompleteMultipartUpload><Part><PartNumber>${part.part_number}</PartNumber><ETag>${etag}</ETag></Part></CompleteMultipartUpload>`,
    });
    expect(done.ok).toBe(true);

    const got = await blob.get(begun.id);
    expect(got.blob.status).toBe("ready");
    expect(new TextDecoder().decode(await blob.bytes(begun.id))).toBe("part one.part two.");
  });
});
