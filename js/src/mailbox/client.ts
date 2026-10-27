import { Http, seg } from "../http.js";
import type {
  AddressPage,
  AddressRecord,
  AddressWithFull,
  AttachmentContent,
  AttachmentLink,
  CreateAddressRequest,
  DownloadLink,
  MailboxInfo,
  MailMessage,
  MessageListQuery,
  MessagePage,
  MessageSummary,
  OutgoingAttachment,
  ReplyRequest,
  SendRequest,
  WaitQuery,
  WaitResult,
} from "./types.js";

/** The longest the server holds a `wait`. */
export const MAX_WAIT_SECONDS = 30;

function bytesToBase64(bytes: Uint8Array): string {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(s);
}

/** Attachment content as the API takes it: a string is already base64; bytes are encoded. */
export async function attachmentBase64(content: AttachmentContent): Promise<string> {
  if (typeof content === "string") return content;
  if (content instanceof Uint8Array) return bytesToBase64(content);
  if (content instanceof ArrayBuffer) return bytesToBase64(new Uint8Array(content));
  if (ArrayBuffer.isView(content)) return bytesToBase64(new Uint8Array(content.buffer, content.byteOffset, content.byteLength));
  if (typeof Blob !== "undefined" && content instanceof Blob) return bytesToBase64(new Uint8Array(await content.arrayBuffer()));
  throw new TypeError("attachment content must be base64 text, a Uint8Array, an ArrayBuffer or a Blob");
}

async function encodeAttachments(list: OutgoingAttachment[] | undefined) {
  if (!list) return undefined;
  return Promise.all(list.map(async (a) => ({ ...a, content: await attachmentBase64(a.content) })));
}

/**
 * Server-side client for one mailbox instance — email addresses an app or agent owns.
 *
 * ORGANIZATION API KEY ONLY: a mailbox holds third parties' mail and can send as you, so
 * end-user identity tokens are refused.
 *
 * Every name works: `<slug>.<anything>@<domain>` receives mail with no call needed, unless the
 * instance accepts registered names only. Every message expires after the mailbox's retention;
 * `keep` moves one message's date.
 */
export class MailboxClient {
  readonly instance: string;
  private readonly http: Http;
  private readonly base: string;

  /** Names that carry a setting: registered, labelled or disabled. */
  readonly addresses: {
    /** One page, in name order. */
    list(query?: { cursor?: string; limit?: number }): Promise<AddressPage>;
    /** Register (and optionally label) a name; omit `local` to have an unguessable one minted. */
    create(req?: CreateAddressRequest): Promise<AddressWithFull>;
    /** Refuse the name's mail and stop it sending. Its messages stay. */
    disable(local: string): Promise<AddressRecord>;
  };

  constructor(http: Http, instance: string) {
    this.http = http;
    this.instance = instance;
    this.base = `/v1/mailbox/${seg(instance)}`;
    this.addresses = {
      list: (query = {}) => this.http.request("GET", `${this.base}/addresses`, { query }),
      create: (req = {}) => this.http.request("POST", `${this.base}/addresses`, { body: req }),
      disable: (local) => this.http.request("DELETE", `${this.base}/addresses/${seg(local)}`),
    };
  }

  /** The main address, the pattern of every other one, and the settings a caller needs. */
  async info(): Promise<MailboxInfo> {
    return this.http.request("GET", this.base);
  }

  /** One page of messages, newest first, without bodies. */
  async list(query: MessageListQuery = {}): Promise<MessagePage> {
    const { unread, ...rest } = query;
    return this.http.request("GET", `${this.base}/messages`, { query: { ...rest, unread: unread ? "1" : undefined } });
  }

  /**
   * Wait for the next inbound mail after `after`, holding the request up to `timeoutSeconds`
   * (at most 30). Empty on timeout — call again with the returned `cursor` to keep waiting.
   *
   * Pass `after: "now"` BEFORE handing an address to whoever will mail it, so older mail is not
   * mistaken for the new one.
   */
  async wait(query: WaitQuery): Promise<WaitResult> {
    const secs = Math.min(Math.max(query.timeoutSeconds ?? MAX_WAIT_SECONDS, 0), MAX_WAIT_SECONDS);
    return this.http.request("GET", `${this.base}/messages`, {
      query: { after: query.after, to: query.to, limit: query.limit, wait: secs },
      // The server holds the request; give it that long and some.
      timeoutMs: secs * 1000 + 15_000,
    });
  }

  /** One message with its bodies and attachment list. */
  async get(id: string): Promise<MailMessage> {
    const { message } = await this.http.request<{ message: MailMessage }>("GET", `${this.base}/messages/${seg(id)}`);
    return message;
  }

  /** A short-lived link to the raw .eml. Fails when the mailbox does not keep raw messages. */
  async raw(id: string): Promise<DownloadLink> {
    const r = await this.http.request<{ rawUrl: string; expiresAt: number }>("GET", `${this.base}/messages/${seg(id)}`, {
      query: { raw: "1" },
    });
    return { url: r.rawUrl, expiresAt: r.expiresAt };
  }

  /** A short-lived download link for one attachment. The URL is the credential: no key needed. */
  async attachment(id: string, idx: number): Promise<AttachmentLink> {
    return this.http.request("GET", `${this.base}/messages/${seg(id)}/attachments/${seg(idx)}`);
  }

  /** Mark a received message read. */
  async markRead(id: string): Promise<{ ok: true }> {
    return this.http.request("POST", `${this.base}/messages/${seg(id)}/read`);
  }

  /**
   * Send a new message from a name on this mailbox. Refused when the send mode is `off`, and in
   * `reply` mode unless every recipient has written to this mailbox in authenticated mail.
   * Never retried: a retried send is a second email.
   */
  async send(req: SendRequest): Promise<MessageSummary> {
    const body = { ...req, attachments: await encodeAttachments(req.attachments) };
    const { message } = await this.http.request<{ message: MessageSummary }>("POST", `${this.base}/send`, {
      body,
      retry: false,
    });
    return message;
  }

  /** Reply to a received message, threaded: to its Reply-To (or From), from the name it was
   *  sent to, with `Re: <subject>` — unless you pass those. Same limits as `send`. */
  async reply(id: string, req: ReplyRequest = {}): Promise<MessageSummary> {
    const body = { ...req, attachments: await encodeAttachments(req.attachments) };
    const { message } = await this.http.request<{ message: MessageSummary }>("POST", `${this.base}/messages/${seg(id)}/reply`, {
      body,
      retry: false,
    });
    return message;
  }

  /**
   * Keep one message until `days` (1–90) from now, overriding the mailbox's retention for it
   * alone. Extending needs `write`; an earlier date deletes the message sooner, so that needs
   * `full`.
   */
  async keep(id: string, days: number): Promise<MessageSummary> {
    const { message } = await this.http.request<{ message: MessageSummary }>("POST", `${this.base}/messages/${seg(id)}/keep`, {
      body: { days },
    });
    return message;
  }

  /** Delete a message and its attachments now. Needs `full`. */
  async delete(id: string): Promise<{ deleted: number }> {
    return this.http.request("DELETE", `${this.base}/messages/${seg(id)}`);
  }
}
