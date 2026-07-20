import { beforeAll, describe, expect, it } from "vitest";
import { AltEngine, AltEngineError, AuthClient, memoryStorage } from "../../src/index.js";
import { baseUrl, client, isEmulator, uniq } from "./helpers.js";

// Auth is the one service whose endpoints are PUBLIC — the end user's own credentials are
// the trust boundary, not an org API key. The id_token it issues is what data-plane calls
// carry, scoped by the auth instance's row rules.

const authInstance = uniq("sdk-auth");
const dsInstance = uniq("sdk-authds");

/** Row rules for the suite: `posts` is read-all/edit-own with stamped authorship;
 * `notes` is owner-private; `secrets` is deliberately absent (default-deny). */
const accessConfig = {
  [`datastore:${dsInstance}`]: {
    level: "full",
    rules: {
      posts: {
        read: "authenticated",
        create: { stamp: { author_uid: "$auth.uid", author_name: "$auth.claims.name" } },
        update: {
          match: [{ field: "author_uid", op: "=", value: "$auth.uid" }],
          immutable: ["author_uid"],
        },
        delete: { match: [{ field: "author_uid", op: "=", value: "$auth.uid" }] },
      },
      notes: {
        read: [{ field: "owner", op: "=", value: "$auth.uid" }],
        create: { stamp: { owner: "$auth.uid" } },
      },
    },
  },
};

const authOpts = () => ({ instance: authInstance, baseUrl: baseUrl(), storage: memoryStorage() });

/** A fresh, isolated client (its own storage) so two users can be signed in at once. */
const newAuth = () => new AuthClient(authOpts());

/** Sign up a user and return their client. */
async function signedUp(email: string, name: string): Promise<AuthClient> {
  const a = newAuth();
  await a.signUp({ email, name, password: "hunter2-long-enough" });
  return a;
}

const raw = (path: string, init?: RequestInit) =>
  fetch(`${baseUrl()}/v1/auth/${authInstance}${path}`, {
    ...init,
    headers: { "content-type": "application/json", ...(init?.headers ?? {}) },
  });

beforeAll(async () => {
  // On the emulator we provision the instance ourselves; a hosted target must have the
  // instance configured in the console (same shape) before running this suite.
  if (!isEmulator()) return;
  // Instances materialize on first use, so touch the public config endpoint to create the
  // auth instance before configuring it (and the datastore instance the rules point at).
  await raw("/config");
  await fetch(`${baseUrl()}/admin/datastore`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ name: dsInstance }),
  });
  // The admin API addresses instances by id, so resolve our name to one.
  const list = (await (await fetch(`${baseUrl()}/admin/auth`)).json()) as {
    instances: { id: string; name: string }[];
  };
  const id = list.instances.find((i) => i.name === authInstance)?.id;
  if (!id) throw new Error(`auth instance ${authInstance} was not created`);
  const res = await fetch(`${baseUrl()}/admin/auth/${id}/config`, {
    method: "PUT",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      config: {
        allowSignup: true,
        passwordlessEnabled: true,
        signup: {
          identityField: "email",
          fields: [
            { key: "email", type: "email", required: true },
            { key: "name", type: "string", required: true },
          ],
        },
        access: accessConfig,
      },
    }),
  });
  if (!res.ok) throw new Error(`could not provision auth instance: ${res.status} ${await res.text()}`);
});

describe("auth: public config", () => {
  it("is fetchable unauthenticated and exposes no secrets", async () => {
    const cfg = await newAuth().config();
    expect(cfg.identity_field).toBe("email");
    expect(cfg.fields.map((f) => f.key)).toContain("email");
    expect(cfg.methods.password).toBe(true);
    // Nothing secret may appear in a public payload. (`methods.password` is a legitimate
    // capability flag, so match secret-bearing KEYS rather than the word "password".)
    const blob = JSON.stringify(cfg);
    expect(blob).not.toMatch(/"(signing_secret|secret|pw_hash|password_hash|captcha_secret)"/i);
  });
});

describe("auth: sign-up and sign-in", () => {
  it("signs up, puts non-identity fields on claims, and signs back in", async () => {
    const email = `${uniq("alice")}@example.com`;
    const a = await signedUp(email, "Alice");
    expect(a.isSignedIn).toBe(true);
    expect(a.user?.identifier).toBe(email);
    expect(a.user?.claims?.name).toBe("Alice");

    const b = newAuth();
    const res = await b.signIn(email, "hunter2-long-enough");
    expect(res.status).toBe("signed_in");
    expect(b.user?.uid).toBe(a.user?.uid);
  });

  it("rejects a duplicate identifier", async () => {
    const email = `${uniq("dup")}@example.com`;
    await signedUp(email, "First");
    const err = await newAuth()
      .signUp({ email, name: "Second", password: "hunter2-long-enough" })
      .catch((e) => e);
    expect(err).toBeInstanceOf(AltEngineError);
    expect(err.code).toBe("ALREADY_EXISTS");
  });

  it("gives the same error for a wrong password and an unknown account", async () => {
    const email = `${uniq("enum")}@example.com`;
    await signedUp(email, "Enum");
    const wrongPw = await newAuth().signIn(email, "not-the-password").catch((e) => e);
    const unknown = await newAuth().signIn(`${uniq("ghost")}@example.com`, "whatever").catch((e) => e);
    expect(wrongPw.status).toBe(unknown.status);
    expect(wrongPw.code).toBe(unknown.code);
    expect(wrongPw.message).toBe(unknown.message); // no account enumeration
  });
});

describe("auth: tokens", () => {
  it("returns the user for /me and refuses without a token", async () => {
    const a = await signedUp(`${uniq("me")}@example.com`, "Me");
    const me = await a.me();
    expect(me.uid).toBe(a.user?.uid);

    const res = await raw("/me");
    expect(res.status).toBe(401);
  });

  it("rotates the refresh token, retiring the old one", async () => {
    const a = await signedUp(`${uniq("rot")}@example.com`, "Rot");
    const oldRefresh = (a as any).session.refresh_token as string;

    await a.refresh();
    // The REFRESH token is what must rotate. The id_token deliberately isn't asserted to
    // differ: it carries the same claims and a whole-second `exp`, so a refresh in the
    // same second legitimately mints byte-identical bytes.
    expect((a as any).session.refresh_token).not.toBe(oldRefresh);

    // The retired refresh token no longer works.
    const res = await raw("/token/refresh", { method: "POST", body: JSON.stringify({ refresh_token: oldRefresh }) });
    expect(res.status).toBe(401);
  });

  it("revokes the refresh token on sign-out", async () => {
    const a = await signedUp(`${uniq("out")}@example.com`, "Out");
    const refresh = JSON.parse(JSON.stringify((a as any).session)).refresh_token as string;
    await a.signOut();
    expect(a.isSignedIn).toBe(false);

    const res = await raw("/token/refresh", { method: "POST", body: JSON.stringify({ refresh_token: refresh }) });
    expect(res.status).toBe(401);
  });
});

describe("auth: passwordless", () => {
  it("signs in with an emailed code and never reveals whether an account exists", async () => {
    const email = `${uniq("pwl")}@example.com`;
    await signedUp(email, "Pwl");

    // The emulator echoes the code as `dev_code` (it can't send mail); a hosted target
    // never does, so this scenario is emulator-only.
    const started = await raw("/passwordless/start", { method: "POST", body: JSON.stringify({ identifier: email }) });
    expect(started.status).toBe(200);
    const body = (await started.json()) as { dev_code?: string };
    if (!isEmulator() || !body.dev_code) return;

    const a = newAuth();
    const res = await a.passwordlessVerify(email, body.dev_code);
    expect(res.status).toBe("signed_in");
    expect(a.user?.identifier).toBe(email);

    // An unknown identifier still returns 200 — no enumeration.
    const ghost = await raw("/passwordless/start", {
      method: "POST",
      body: JSON.stringify({ identifier: `${uniq("ghost")}@example.com` }),
    });
    expect(ghost.status).toBe(200);
  });
});

describe("auth: identity tokens on the data plane", () => {
  it("stamps authorship from the token, ignoring a forged author in the body", async () => {
    const alice = await signedUp(`${uniq("a")}@example.com`, "Alice");
    const db = new AltEngine({ baseUrl: baseUrl(), auth: alice }).datastore(dsInstance);

    const { keys } = await db.put("posts", [{ data: { title: "hello", author_uid: "somebody-else" } }]);
    const got = await db.get("posts", keys);
    const doc = (Array.isArray(got) ? got[0] : got) as any;
    expect(doc.data.author_uid).toBe(alice.user?.uid); // forged value overwritten
    expect(doc.data.author_name).toBe("Alice"); // stamped from a claim
  });

  it("refuses another user's update, delete, and immutable-field change", async () => {
    const alice = await signedUp(`${uniq("a")}@example.com`, "Alice");
    const bob = await signedUp(`${uniq("b")}@example.com`, "Bob");
    const aliceDb = new AltEngine({ baseUrl: baseUrl(), auth: alice }).datastore(dsInstance);
    const bobDb = new AltEngine({ baseUrl: baseUrl(), auth: bob }).datastore(dsInstance);

    const { keys } = await aliceDb.put("posts", [{ data: { title: "alice's" } }]);
    const key = keys[0]!;

    const upd = await bobDb.put("posts", [{ key, data: { title: "hijacked" } }]).catch((e) => e);
    expect(upd.code).toBe("PERMISSION_DENIED");

    const del = await bobDb.delete("posts", [key]).catch((e) => e);
    expect(del.code).toBe("PERMISSION_DENIED");

    // Alice owns it, but author_uid is immutable.
    const imm = await aliceDb
      .put("posts", [{ key, data: { title: "t", author_uid: bob.user!.uid } }])
      .catch((e) => e);
    expect(imm.code).toBe("PERMISSION_DENIED");
  });

  it("scopes an owner-private read to the caller's own rows", async () => {
    const alice = await signedUp(`${uniq("a")}@example.com`, "Alice");
    const bob = await signedUp(`${uniq("b")}@example.com`, "Bob");
    const aliceDb = new AltEngine({ baseUrl: baseUrl(), auth: alice }).datastore(dsInstance);
    const bobDb = new AltEngine({ baseUrl: baseUrl(), auth: bob }).datastore(dsInstance);

    await aliceDb.put("notes", [{ data: { body: "alice only" } }]);
    await bobDb.put("notes", [{ data: { body: "bob only" } }]);

    const mine = await aliceDb.query("notes", { limit: 50 });
    expect(mine.documents!.length).toBeGreaterThan(0);
    for (const d of mine.documents!) expect((d.data as any).owner).toBe(alice.user?.uid);
  });

  it("default-denies a collection with no rule", async () => {
    const alice = await signedUp(`${uniq("a")}@example.com`, "Alice");
    const db = new AltEngine({ baseUrl: baseUrl(), auth: alice }).datastore(dsInstance);
    const err = await db.query("secrets").catch((e) => e);
    expect(err.code).toBe("PERMISSION_DENIED");
  });

  it("default-denies a datastore instance with no access entry", async () => {
    const alice = await signedUp(`${uniq("a")}@example.com`, "Alice");
    const other = new AltEngine({ baseUrl: baseUrl(), auth: alice }).datastore(uniq("sdk-unlisted"));
    const err = await other.query("posts").catch((e) => e);
    expect(err.code).toBe("PERMISSION_DENIED");
  });

  it("refuses backend-only surfaces to an identity token", async () => {
    const alice = await signedUp(`${uniq("a")}@example.com`, "Alice");
    const db = new AltEngine({ baseUrl: baseUrl(), auth: alice }).datastore(dsInstance);
    const err = await db.transaction([{ op: "put", collection: "posts", data: { title: "x" } } as any]).catch((e) => e);
    expect(err.code).toBe("PERMISSION_DENIED");
  });
});

describe("auth: org API key is unaffected", () => {
  it("still bypasses row rules (full backend trust)", async () => {
    const db = client().datastore(dsInstance);
    const { keys } = await db.put("secrets", [{ data: { classified: true } }]);
    expect(keys).toHaveLength(1);
    const got = await db.query("secrets", { limit: 1 });
    expect(got.documents!.length).toBe(1);
  });
});
