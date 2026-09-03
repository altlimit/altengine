import { describe, expect, it, vi } from "vitest";
import { AltEngine, blobFromJobEnv, type BlobRecord } from "../../src/index.js";

// The blob client is thin over REST except for the transfer, which is where the behaviour lives:
// the bytes never go through the API, and a multipart upload that fails must not leave parts
// behind. What is worth asserting is what a caller would otherwise get wrong.

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

function client(fetchImpl: typeof fetch) {
  return new AltEngine({ baseUrl: "http://test.local", apiKey: "ae_secret", fetch: fetchImpl });
}

const record = (over: Partial<BlobRecord> = {}): BlobRecord =>
  ({
    id: "b1",
    name: "hello.txt",
    size: 11,
    content_type: "text/plain",
    etag: "e",
    status: "ready",
    public: false,
    created: 1,
    updated: 1,
    meta: {},
    blobkey: "blob:i1:b1",
    url: null,
    ...over,
  }) as BlobRecord;

const minted = {
  id: "b1",
  blobkey: "blob:i1:b1",
  upload_url: "https://storage.example/obj?X-Amz-Signature=abc",
  expires_at: 2,
  required_headers: { "content-length": "11", "content-type": "text/plain" },
  method: "PUT" as const,
};

describe("uploading", () => {
  it("reserves, PUTs straight to storage, and reads back the finished object", async () => {
    const seen: string[] = [];
    const fetchMock = vi.fn(async (url: any, init: any) => {
      seen.push(`${init?.method ?? "GET"} ${String(url)}`);
      if (String(url).endsWith("/uploads")) return json(201, minted);
      if (String(url).startsWith("https://storage.example")) return new Response(null, { status: 200 });
      return json(200, { blob: record(), download_url: "https://storage.example/get", expires_at: 3 });
    });
    const out = await client(fetchMock as never).blob("files").put("hello.txt", "hello world", { contentType: "text/plain" });
    expect(out.blobkey).toBe("blob:i1:b1");
    expect(seen).toEqual([
      "POST http://test.local/v1/blob/files/uploads",
      "PUT https://storage.example/obj?X-Amz-Signature=abc",
      "GET http://test.local/v1/blob/files/b1",
    ]);
  });

  it("never sends the API key to storage", async () => {
    // The signature IS the credential on a presigned URL. An org key on that request would be
    // an org key handed to a third party, and it would be handed over on every upload.
    let storageHeaders: Headers | undefined;
    const fetchMock = vi.fn(async (url: any, init: any) => {
      if (String(url).endsWith("/uploads")) return json(201, minted);
      if (String(url).startsWith("https://storage.example")) {
        storageHeaders = new Headers(init?.headers);
        return new Response(null, { status: 200 });
      }
      return json(200, { blob: record(), download_url: "https://storage.example/get", expires_at: 3 });
    });
    await client(fetchMock as never).blob("files").put("hello.txt", "hello world");
    expect(storageHeaders?.get("authorization")).toBeNull();
  });

  it("declares the exact byte length it is about to send", async () => {
    // The size is signed into the URL. A reservation for a length other than the body's is
    // refused by storage with an error that reads like a clock problem.
    let body: unknown;
    const fetchMock = vi.fn(async (url: any, init: any) => {
      if (String(url).endsWith("/uploads")) {
        body = JSON.parse(init.body);
        return json(201, minted);
      }
      if (String(url).startsWith("https://storage.example")) return new Response(null, { status: 200 });
      return json(200, { blob: record(), download_url: "u", expires_at: 3 });
    });
    await client(fetchMock as never).blob("files").put("hello.txt", "hello world", { contentType: "text/plain" });
    expect(body).toEqual({ name: "hello.txt", size: 11, content_type: "text/plain" });
  });

  it("sends `public` rather than deciding it here, so a job token is refused rather than downgraded", async () => {
    // A container job may not publish, and the refusal is the server's to make. Dropping the
    // flag would store the object privately and report success.
    const fetchMock = vi.fn(async (url: any, init: any) => {
      if (String(url).endsWith("/uploads")) {
        expect(JSON.parse(init.body).public).toBe(true);
        return json(403, { error: { code: "PERMISSION_DENIED", message: "a container job may not publish an object" } });
      }
      return json(200, {});
    });
    await expect(
      client(fetchMock as never).blob("files").put("x.txt", "x", { public: true })
    ).rejects.toThrow(/may not publish/);
  });

  it("does not retry a reservation", async () => {
    // Each retry is another pending object and another slot in the store.
    let calls = 0;
    const fetchMock = vi.fn(async () => {
      calls++;
      return json(503, { error: { code: "UNAVAILABLE", message: "try later" } });
    });
    await client(fetchMock as never).blob("files").uploadUrl({ size: 1 }).catch(() => {});
    expect(calls).toBe(1);
  });
});

describe("a multipart upload", () => {
  const begun = {
    id: "b2",
    blobkey: "blob:i1:b2",
    key: "i1/b2",
    part_size: 5,
    parts: 2,
    create_url: "https://storage.example/obj?uploads=",
    expires_at: 9,
  };
  const urls = {
    part_urls: [
      { part_number: 1, url: "https://storage.example/obj?partNumber=1" },
      { part_number: 2, url: "https://storage.example/obj?partNumber=2" },
    ],
    complete_url: "https://storage.example/obj?uploadId=u1",
    abort_url: "https://storage.example/obj?abort=1",
    expires_at: 9,
  };

  /** A file large enough that the client must cut it up, without allocating one. */
  const hugeBlob = (size: number): Blob =>
    ({
      size,
      type: "application/zip",
      slice: (a: number, b: number) => ({ size: b - a, type: "" }) as Blob,
    }) as unknown as Blob;

  it("creates, sends every part, and names their ETags in order", async () => {
    let completeBody = "";
    const fetchMock = vi.fn(async (url: any, init: any) => {
      const u = String(url);
      if (u.endsWith("/uploads/multipart")) return json(201, begun);
      if (u.endsWith("/uploads/multipart/urls")) return json(200, urls);
      if (u.includes("uploads=")) return new Response("<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>", { status: 200 });
      if (u.includes("partNumber=")) {
        const n = u.slice(-1);
        return new Response(null, { status: 200, headers: { etag: `"tag-${n}"` } });
      }
      if (u.includes("uploadId=u1")) {
        completeBody = init.body;
        return new Response("<CompleteMultipartUploadResult/>", { status: 200 });
      }
      return json(200, { blob: record({ id: "b2" }), download_url: "u", expires_at: 3 });
    });

    // 6 GB: over what one request can carry, so the client has no choice about the shape.
    const out = await client(fetchMock as never).blob("files").put("export.zip", hugeBlob(6 * 1024 ** 3));
    expect(out.id).toBe("b2");
    expect(completeBody).toBe(
      '<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"tag-1"</ETag></Part>' +
        '<Part><PartNumber>2</PartNumber><ETag>"tag-2"</ETag></Part></CompleteMultipartUpload>'
    );
  });

  it("aborts when a part fails, and reports the part's failure rather than the tidy-up", async () => {
    // Parts of an upload nobody completed keep billing and no listing shows them.
    const seen: string[] = [];
    const fetchMock = vi.fn(async (url: any, init: any) => {
      const u = String(url);
      seen.push(`${init?.method ?? "GET"} ${u}`);
      if (u.endsWith("/uploads/multipart")) return json(201, begun);
      if (u.endsWith("/uploads/multipart/urls")) return json(200, urls);
      if (u.includes("uploads=")) return new Response("<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>", { status: 200 });
      if (u.includes("partNumber=")) return new Response("<Error><Code>SignatureDoesNotMatch</Code><Message>bad sig</Message></Error>", { status: 403 });
      return new Response(null, { status: 204 });
    });

    await expect(
      client(fetchMock as never).blob("files").put("export.zip", hugeBlob(6 * 1024 ** 3))
    ).rejects.toThrow(/bad sig/);
    expect(seen).toContain("DELETE https://storage.example/obj?abort=1");
  });

  it("refuses to complete an upload whose parts have no ETag", async () => {
    // In a browser this is the CORS configuration talking. Completing without them would name
    // parts storage cannot match and produce a corrupt object nobody can explain.
    const fetchMock = vi.fn(async (url: any) => {
      const u = String(url);
      if (u.endsWith("/uploads/multipart")) return json(201, begun);
      if (u.endsWith("/uploads/multipart/urls")) return json(200, urls);
      if (u.includes("uploads=")) return new Response("<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>", { status: 200 });
      if (u.includes("partNumber=")) return new Response(null, { status: 200 });
      return new Response(null, { status: 204 });
    });
    await expect(
      client(fetchMock as never).blob("files").put("export.zip", hugeBlob(6 * 1024 ** 3))
    ).rejects.toThrow(/exposed response header/);
  });
});

describe("reading", () => {
  it("fetches bytes from storage, not through the API", async () => {
    const seen: string[] = [];
    const fetchMock = vi.fn(async (url: any) => {
      seen.push(String(url));
      if (String(url).startsWith("http://test.local")) {
        return json(200, { blob: record(), download_url: "https://storage.example/get?sig=x", expires_at: 3 });
      }
      return new Response(new TextEncoder().encode("hello world"), { status: 200 });
    });
    const bytes = await client(fetchMock as never).blob("files").bytes("b1");
    expect(new TextDecoder().decode(bytes)).toBe("hello world");
    expect(seen[1]).toBe("https://storage.example/get?sig=x");
  });

  it("hands back the cursor rather than pretending the page is the store", async () => {
    const fetchMock = vi.fn(async () => json(200, { blobs: [record()], cursor: "1:b1" }));
    const page = await client(fetchMock as never).blob("files").list({ prefix: "invoices/" });
    expect(page.cursor).toBe("1:b1");
  });

  it("walks every page, carrying the cursor", async () => {
    const pages = [
      { blobs: [record({ id: "a" })], cursor: "1:a" },
      { blobs: [record({ id: "b" })], cursor: null },
    ];
    let n = 0;
    const seen: string[] = [];
    const fetchMock = vi.fn(async (url: any) => {
      seen.push(String(url));
      return json(200, pages[n++]);
    });
    const ids: string[] = [];
    for await (const b of client(fetchMock as never).blob("files").listAll({ prefix: "p/" })) ids.push(b.id);
    expect(ids).toEqual(["a", "b"]);
    expect(seen[1]).toContain("cursor=1%3Aa");
    // The filter has to travel on every page, or page two silently widens to the whole store.
    expect(seen.every((u) => u.includes("prefix=p%2F"))).toBe(true);
  });

  it("surfaces a job token's refusal to list rather than returning an empty store", async () => {
    const fetchMock = vi.fn(async () =>
      json(403, { error: { code: "PERMISSION_DENIED", message: "a container job may not list the objects in a store" } })
    );
    await expect(client(fetchMock as never).blob("files").list()).rejects.toThrow(/may not list/);
  });
});

describe("a container job's own client", () => {
  it("splits the platform's URL into an origin and an instance, and carries the job token", async () => {
    process.env.AE_BLOB_URL = "https://api.altengine.net/v1/blob/run-output";
    process.env.AE_BLOB_TOKEN = "jbt_abc";
    try {
      let auth: string | null = null;
      const fetchMock = vi.fn(async (url: any, init: any) => {
        expect(String(url)).toBe("https://api.altengine.net/v1/blob/run-output/b1");
        auth = new Headers(init?.headers).get("authorization");
        return json(200, { blob: record(), download_url: "u", expires_at: 1 });
      });
      const blob = blobFromJobEnv({ fetch: fetchMock as never });
      expect(blob.instance).toBe("run-output");
      await blob.get("b1");
      expect(auth).toBe("Bearer jbt_abc");
    } finally {
      delete process.env.AE_BLOB_URL;
      delete process.env.AE_BLOB_TOKEN;
    }
  });

  it("says so when the instance names no store, rather than half-building a client", async () => {
    delete process.env.AE_BLOB_URL;
    delete process.env.AE_BLOB_TOKEN;
    expect(() => blobFromJobEnv()).toThrow(/blobStore/);
  });
});
