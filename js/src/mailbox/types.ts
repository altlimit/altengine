/** Wire types for the mailbox data plane — email addresses an app or agent owns. Unlike the
 *  older services, this API's fields are camelCase. */

/** One mailbox: an address and a display name. */
export interface MailAddress {
  address: string;
  name: string;
}

/** Why the service stopped this mailbox's sending: too many sends today went to addresses that
 *  previously bounced or marked mail as spam. Only an owner can resume it, in the console. */
export interface SendPause {
  at: number;
  suppressed: number;
  reason: string;
}

export interface MailboxInfo {
  name: string;
  /** The mail domain, or null when none is configured. */
  domain: string | null;
  /** The main address (`<slug>@<domain>`), or null when the instance has none. */
  address: string | null;
  /** Every other address: `<slug>.<anything>@<domain>`. */
  pattern: string | null;
  acceptMode: "any" | "registered";
  sendMode: "off" | "reply" | "any";
  retentionDays: number;
  /** Null unless sending is paused; sends then fail with 403 SEND_PAUSED. */
  sendPaused: SendPause | null;
}

/** A message as every list returns it: everything but the bodies. */
export interface MessageSummary {
  id: string;
  direction: "in" | "out";
  /** The name the message was sent to (received) or from (sent): `inbox` for the main address. */
  local: string;
  plusTag: string | null;
  from: MailAddress;
  to: MailAddress[];
  cc: MailAddress[];
  replyTo: MailAddress[];
  subject: string;
  snippet: string;
  messageId: string | null;
  inReplyTo: string | null;
  /** Whether the sender's domain authenticated a received message. Reply mode answers only
   *  senders that did. */
  verified: boolean;
  status: string;
  size: number;
  attachmentCount: number;
  receivedAt: number;
  readAt: number | null;
  /** When the message and its attachments are deleted. Move it with `keep`. */
  expiresAt: number;
}

export interface MessageAttachment {
  idx: number;
  filename: string | null;
  contentType: string;
  size: number;
  contentId: string | null;
  disposition: string | null;
  /** Why the part was not stored (too large, too many), or null. */
  rejected: string | null;
  /** Whether `attachment(id, idx)` can hand out a download link for it. */
  stored: boolean;
}

/** What one route action did for a received message. */
export interface MailDelivery {
  /** `function:<instance>/<fn>`, `channel:<instance>/<channel>`, `channel+text:…` or `webhook:<url>`. */
  action: string;
  /** `pending`, `ok:<status>`, or `error:<why>`. */
  status: string;
  attempts: number;
  nextAt: number | null;
}

/** One full message. */
export interface MailMessage extends MessageSummary {
  text: string | null;
  html: string | null;
  textTruncated: boolean;
  htmlTruncated: boolean;
  references: string | null;
  authResults: string | null;
  deliveries: MailDelivery[];
  /** Whether the raw .eml was kept, so `raw` can link to it. */
  hasRaw: boolean;
  attachments: MessageAttachment[];
}

export interface MessagePage {
  messages: MessageSummary[];
  /** Newest-first paging: pass back as `cursor`. Null when there is nothing older. */
  cursor: string | null;
}

/** A type alias rather than an interface so it satisfies the query-string record. */
export type MessageListQuery = {
  /** Only mail for this name (`support`) or full address. */
  to?: string;
  direction?: "in" | "out";
  /** Only received mail not yet marked read. */
  unread?: boolean;
  cursor?: string;
  limit?: number;
};

export interface WaitQuery {
  /** The point to wait from: `"now"` (mail from this moment on), a message id or a previous
   *  `cursor`, or `"0"` for any message still held. Required — so last week's mail is never
   *  mistaken for the one you are waiting for. */
  after: string;
  to?: string;
  /** How long the server holds the request, at most 30 (the default). */
  timeoutSeconds?: number;
  limit?: number;
}

export interface WaitResult {
  /** Inbound messages newer than `after`, oldest first; empty on timeout. */
  messages: MessageSummary[];
  /** Where to wait from next: the last message's id, or `after` unchanged. */
  cursor: string;
}

/** A short-lived download link. */
export interface DownloadLink {
  url: string;
  expiresAt: number;
}

export interface AttachmentLink extends DownloadLink {
  filename: string | null;
  contentType: string;
  size: number;
}

/** File content for an attachment: base64 text, or bytes the SDK base64-encodes for you. */
export type AttachmentContent = string | Uint8Array | ArrayBuffer | ArrayBufferView | Blob;

export interface OutgoingAttachment {
  /** Defaults to `attachment-<n>`. */
  filename?: string;
  /** Defaults to `application/octet-stream`. */
  contentType?: string;
  /** A string is taken as base64 already; bytes are encoded. */
  content: AttachmentContent;
  /** Set to show the part inline, referenced from the HTML as `cid:<contentId>`
   *  (1–100 letters, digits, `.`, `_`, `@` or `-`). */
  contentId?: string;
}

export interface SendRequest {
  /** The name to send from: `support` sends as `<slug>.support@<domain>`. Defaults to the
   *  main address. */
  from?: string;
  fromName?: string;
  to: string[] | string;
  cc?: string[] | string;
  subject: string;
  text?: string;
  html?: string;
  /** Up to 32. The whole message must stay under 5 MB once encoded — about 3.6 MB of files. */
  attachments?: OutgoingAttachment[];
}

/** A reply: `to`, `subject` and `from` default from the message being answered. */
export type ReplyRequest = Partial<SendRequest>;

export interface AddressRecord {
  local: string;
  label: string | null;
  meta: Record<string, unknown>;
  registered: boolean;
  createdAt: number;
  disabledAt: number | null;
}

export interface AddressWithFull extends AddressRecord {
  /** The full address, or null when the instance has none. */
  address: string | null;
}

export interface AddressPage {
  addresses: AddressWithFull[];
  cursor: string | null;
}

export interface CreateAddressRequest {
  /** Omit to have an unguessable name minted. */
  local?: string;
  label?: string;
  /** At most 4 KB of JSON. */
  meta?: Record<string, unknown>;
}
