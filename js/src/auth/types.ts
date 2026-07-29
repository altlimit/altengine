/** Auth wire types. Field names are wire-verbatim (snake_case) — they double as
 * the API reference for `/v1/auth/{instance}`. */

/** One field the instance's sign-up form collects. The field whose `key` equals
 * `AuthConfig.identity_field` is the unique login handle; every other field is
 * stored on the user's `claims`. */
export interface AuthField {
  key: string;
  type: "email" | "string" | "number" | "boolean";
  required: boolean;
  label?: string;
}

/** The instance's public, client-safe sign-in configuration (`GET /config`).
 * Safe to fetch before authenticating — it contains no secrets. */
export interface AuthConfig {
  instance: string;
  allow_signup: boolean;
  /** Which collected field is the unique login handle (e.g. "email", "username"). */
  identity_field: string;
  fields: AuthField[];
  captcha: { enabled: boolean; site_key?: string };
  methods: { password: boolean; passkeys: boolean; passwordless: boolean; totp: boolean };
}

/** An end user of your app (not an altengine account).
 *
 * `profile` holds the fields the user supplied at sign-up (name, etc.), read in
 * access rules as `$auth.profile.X` — self-asserted, never authoritative. `claims`
 * is server/admin-set only, read as `$auth.claims.X` — authoritative, safe to
 * authorize on. A user can never write `claims`, which is what makes it safe to
 * base access rules on it. */
export interface AuthUser {
  uid: string;
  identifier: string;
  profile?: Record<string, unknown>;
  claims?: Record<string, unknown>;
  totpEnabled?: boolean;
}

/** Tokens returned by sign-up / sign-in / refresh. `expires_at` and
 * `refresh_expires_at` are unix SECONDS. */
export interface AuthTokens {
  id_token: string;
  refresh_token: string;
  expires_at: number;
  refresh_expires_at: number;
}

/** A completed sign-in: tokens plus the user they belong to. */
export interface AuthSession extends AuthTokens {
  user: AuthUser;
}

/** Returned instead of tokens when the account has two-factor enabled. Exchange
 * `mfa_token` plus the authenticator code at `POST /2fa/verify`. */
export interface MfaChallenge {
  mfa_required: true;
  mfa_token: string;
}

/** The two possible outcomes of a sign-in attempt. */
export type SignInResult =
  | { status: "signed_in"; session: AuthSession }
  | { status: "mfa_required"; mfaToken: string };

/** Persisted session shape (what a `TokenStorage` holds). */
export interface StoredSession {
  id_token: string;
  refresh_token: string;
  expires_at: number;
  user: AuthUser | null;
}
