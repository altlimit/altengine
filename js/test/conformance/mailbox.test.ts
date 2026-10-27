import { beforeAll, describe, expect, it } from "vitest";
import { AltEngineError, type MailboxClient } from "../../src/index.js";
import { apiKey, baseUrl, client, destructiveOk, isEmulator, uniq } from "./helpers.js";

// Mail has to ARRIVE before most of this means anything, and only the emulator can deliver a
// message on request (its /inject route). Against a hosted instance the suite asserts what needs
// no inbound mail.

const instance = uniq("sdk-conf");
let mb: MailboxClient;

beforeAll(() => {
  mb = client().mailbox(instance);
});

/** Deliver a raw message through the emulator's inbound path, as an authenticated sender. */
async function inject(to: string, raw: string): Promise<void> {
  const res = await fetch(`${baseUrl()}/v1/mailbox/${instance}/inject?to=${encodeURIComponent(to)}&authenticated=1`, {
    method: "POST",
    headers: { authorization: `Bearer ${apiKey()}` },
    body: raw,
  });
  if (res.status !== 201) throw new Error(`inject: ${res.status} ${await res.text()}`);
}

const mail = (subject: string) =>
  `From: Jane <jane@example.com>\r\nTo: x@example.com\r\nSubject: ${subject}\r\nMessage-ID: <${uniq("m")}@example.com>\r\n\r\nUse 482913 to sign in.\r\n`;

describe("the instance", () => {
  it("names its addresses and is not paused", async () => {
    const info = await mb.info();
    expect(info.name).toBe(instance);
    expect(info.address).toMatch(new RegExp(`^${instance}@`));
    expect(info.pattern).toMatch(new RegExp(`^${instance}\\.<anything>@`));
    expect(info.sendMode).toBe("reply");
    expect(info.sendPaused).toBeNull();
  });
});

describe("receiving, replying and keeping", () => {
  it("waits for mail, reads it, answers it with an attachment, and keeps it", async () => {
    if (!isEmulator()) return;
    // Parked BEFORE the mail exists, as a caller handing out an address would.
    const waiting = mb.wait({ after: "now", to: "support", timeoutSeconds: 10 });
    await new Promise((r) => setTimeout(r, 200));
    await inject("support", mail("Your code"));
    const got = await waiting;
    expect(got.messages).toHaveLength(1);
    const id = got.messages[0]!.id;
    expect(got.cursor).toBe(id);

    const msg = await mb.get(id);
    expect(msg.subject).toBe("Your code");
    expect(msg.verified).toBe(true);
    expect(msg.text).toContain("482913");

    expect((await mb.list({ unread: true })).messages.map((m) => m.id)).toContain(id);
    await mb.markRead(id);
    expect((await mb.list({ unread: true })).messages.map((m) => m.id)).not.toContain(id);

    const raw = await mb.raw(id);
    const eml = await (await fetch(raw.url)).text();
    expect(eml).toContain("Subject: Your code");

    // The sender authenticated, so reply mode answers them — with bytes and base64 alike.
    const sent = await mb.reply(id, {
      text: "here you go",
      attachments: [
        { filename: "notes.txt", contentType: "text/plain", content: new TextEncoder().encode("hello notes") },
        { filename: "logo.png", contentType: "image/png", content: "iVBORw==", contentId: "logo" },
      ],
    });
    expect(sent.direction).toBe("out");
    expect(sent.subject).toBe("Re: Your code");
    expect(sent.attachmentCount).toBe(2);
    const full = await mb.get(sent.id);
    expect(full.attachments.map((a) => [a.filename, a.disposition, a.stored])).toEqual([
      ["notes.txt", "attachment", true],
      ["logo.png", "inline", true],
    ]);
    const link = await mb.attachment(sent.id, 0);
    expect(link.filename).toBe("notes.txt");
    expect(await (await fetch(link.url)).text()).toBe("hello notes");

    const kept = await mb.keep(id, 90);
    expect(kept.expiresAt).toBeGreaterThan(Date.now() + 89 * 86_400_000);
    await expect(mb.keep(id, 91)).rejects.toThrow(/days must be an integer from 1 to 90/);
    await expect(mb.keep("nope", 3)).rejects.toThrow(/message not found/);

    if (destructiveOk()) {
      expect(await mb.delete(id)).toEqual({ deleted: 1 });
      await expect(mb.get(id)).rejects.toThrow(/not found/);
    }
  });

  it("answers an empty wait with the cursor it was given", async () => {
    const out = await mb.wait({ after: "0", to: "nobody-here", timeoutSeconds: 0 });
    expect(out.messages).toEqual([]);
    expect(out.cursor).toBe("0");
  });
});

describe("sending", () => {
  it("refuses a stranger in reply mode", async () => {
    const err = await mb.send({ to: ["stranger@example.com"], subject: "hi", text: "x" }).catch((e) => e);
    expect(err).toBeInstanceOf(AltEngineError);
    expect((err as AltEngineError).code).toBe("NOT_A_REPLY");
  });

  it("refuses a malformed attachment", async () => {
    const err = await mb
      .send({ to: ["stranger@example.com"], subject: "hi", text: "x", attachments: [{ content: "not base64!" }] })
      .catch((e) => e);
    expect((err as AltEngineError).status).toBe(400);
    expect(String((err as AltEngineError).message)).toMatch(/attachments\[0\]\.content must be base64/);
  });

  it("refuses a message too large to send, before anything else", async () => {
    const err = await mb
      .send({ to: ["stranger@example.com"], subject: "big", text: "x", attachments: [{ filename: "big.bin", content: new Uint8Array(3_900_000) }] })
      .catch((e) => e);
    expect((err as AltEngineError).status).toBe(413);
  });
});

describe("addresses", () => {
  it("registers, lists and disables a name", async () => {
    const minted = await mb.addresses.create({ label: "signup" });
    expect(minted.registered).toBe(true);
    expect(minted.address).toMatch(new RegExp(`^${instance}\\.${minted.local}@`));
    const named = await mb.addresses.create({ local: "billing" });
    expect(named.local).toBe("billing");
    const page = await mb.addresses.list();
    expect(page.addresses.map((a) => a.local)).toEqual(expect.arrayContaining(["billing", minted.local]));
    const off = await mb.addresses.disable("billing");
    expect(off.disabledAt).toBeGreaterThan(0);
  });
});
