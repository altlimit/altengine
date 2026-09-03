import { Http, seg } from "../http.js";
import { sleep } from "../poll.js";
import type {
  Agent,
  Artifact,
  Delivery,
  LogPage,
  Run,
  RunWithArtifacts,
  Schedule,
  Script,
  StartRunRequest,
} from "./types.js";

/** How long `wait` sleeps between polls, and the ceiling it backs off to.
 *
 * A run drives a real desktop for minutes or hours, so polling fast buys nothing and costs a
 * request every time. Starts at two seconds because a browser-only run can genuinely be quick. */
const POLL_START_MS = 2_000;
const POLL_MAX_MS = 15_000;

const TERMINAL = new Set(["done", "failed", "canceled", "abandoned"]);

/**
 * Server-side client for one automation instance.
 *
 * ORGANIZATION API KEY ONLY. End-user identity tokens are refused by this service, as they are by
 * containers: a run spends time on a machine you own, and access rules bound what a user may
 * READ, not what they may spend. Have a page call a function that decides.
 *
 * Scripts are deployed with the CLI (`altengine automation deploy`), not from here — they are
 * bundled into one flat ES module before upload, and shipping a bundler in this package to do it
 * at runtime would be the wrong trade for every caller who never deploys.
 */
export class AutomationClient {
  readonly instance: string;
  private readonly http: Http;
  private readonly base: string;

  constructor(http: Http, instance: string) {
    this.http = http;
    this.instance = instance;
    this.base = `/v1/automation/${seg(instance)}`;
  }

  // --- runs ---------------------------------------------------------------

  /**
   * Start a run and return immediately.
   *
   * A `queued` run is NOT a failure — it means no machine matching the request is online yet, and
   * an office PC being asleep is the ordinary case. It will be dispatched when one connects.
   */
  async start(req: StartRunRequest): Promise<Run> {
    const { run } = await this.http.request<{ run: Run }>("POST", `${this.base}/runs`, {
      body: req,
      // A retried start is a SECOND RUN: it drives a real application, so a duplicate is a second
      // pass over the same invoices rather than a wasted request.
      retry: false,
    });
    return run;
  }

  /** A run and its artifacts, each with a freshly minted download link. */
  async get(runId: string): Promise<RunWithArtifacts> {
    return this.http.request("GET", `${this.base}/runs/${seg(runId)}`);
  }

  async list(
    query: { status?: string; script?: string; agent_id?: string; limit?: number; cursor?: string } = {}
  ): Promise<{ runs: Run[]; cursor: string | null }> {
    return this.http.request("GET", `${this.base}/runs`, { query });
  }

  /** Stop a queued or running run. The script unwinds through its own cleanup. */
  async cancel(runId: string): Promise<{ canceled: boolean }> {
    return this.http.request("POST", `${this.base}/runs/${seg(runId)}/cancel`, { retry: false });
  }

  /**
   * A page of the run's log.
   *
   * While the run is going this is a live tail from the machine — the last few hundred lines, not
   * a transcript. Once it finishes it is the complete log the run uploaded, so a finished run is
   * readable even from a machine that has since been switched off. `source` says which answered.
   */
  async logs(runId: string, query: { cursor?: string } = {}): Promise<LogPage> {
    return this.http.request("GET", `${this.base}/runs/${seg(runId)}/logs`, { query });
  }

  /** The run's artifacts, each with a freshly minted download link. */
  async artifacts(runId: string): Promise<Artifact[]> {
    const { artifacts } = await this.http.request<{ artifacts: Artifact[] }>(
      "GET",
      `${this.base}/runs/${seg(runId)}/artifacts`
    );
    return artifacts;
  }

  /**
   * Poll until a run reaches a terminal status, then return it with its artifacts.
   *
   * THE RUN IS FINISHED WHEN ITS OUTPUT HAS LANDED, not when its script returned — the agent holds
   * the terminal report until every artifact has been stored — so a `done` here always has its
   * files, and the links on them are live.
   *
   * `timeoutMs` bounds the WAIT, not the run: giving up here leaves the run going on the machine.
   * Call `cancel` if that is what you meant. Zero (the default) waits indefinitely, which is
   * usually right for a job that legitimately takes hours.
   */
  async wait(
    runId: string,
    opts: { timeoutMs?: number; signal?: AbortSignal; onPoll?: (run: Run) => void } = {}
  ): Promise<RunWithArtifacts> {
    const deadline = opts.timeoutMs ? Date.now() + opts.timeoutMs : 0;
    let delay = POLL_START_MS;
    for (;;) {
      const current = await this.get(runId);
      opts.onPoll?.(current.run);
      if (TERMINAL.has(current.run.status)) return current;
      if (deadline && Date.now() >= deadline) {
        throw new Error(
          `run ${runId} was still ${current.run.status} after ${opts.timeoutMs}ms — it is still going on the machine; cancel it if that is not what you want`
        );
      }
      let ms = delay;
      if (deadline) ms = Math.min(ms, Math.max(0, deadline - Date.now()));
      await sleep(ms, opts.signal);
      delay = Math.min(delay * 2, POLL_MAX_MS);
    }
  }

  /** Start a run and wait for it. Convenience over `start` + `wait`. */
  async run(
    req: StartRunRequest,
    opts: { timeoutMs?: number; signal?: AbortSignal; onPoll?: (run: Run) => void } = {}
  ): Promise<RunWithArtifacts> {
    const started = await this.start(req);
    return this.wait(started.id, opts);
  }

  // --- machines, scripts, schedules ---------------------------------------

  /** Enrolled machines, with whether each is connected right now. */
  /**
   * One page of enrolled machines, with whether each is connected right now.
   *
   * PAGED, and the cursor matters: a fleet of hundreds is the ordinary shape of this service —
   * an office, a chain of dealerships — and a caller that ignores it is looking at part of one.
   * `q` matches name, hostname or label, which are the three things a machine is known by.
   */
  async agents(
    query: { cursor?: string; q?: string; limit?: number } = {}
  ): Promise<{ agents: Agent[]; cursor: string | null }> {
    return this.http.request("GET", `${this.base}/agents`, { query });
  }

  /**
   * Every enrolled machine, walking the pages for you.
   *
   * Here because the alternative — a `agents()` that quietly returned the first page — is how a
   * fleet with more machines than a page silently became a smaller fleet. If you are counting
   * machines, or acting on all of them, use this one.
   */
  async allAgents(query: { q?: string } = {}): Promise<Agent[]> {
    const out: Agent[] = [];
    let cursor: string | undefined;
    for (;;) {
      const page = await this.agents({ ...query, cursor });
      out.push(...page.agents);
      if (!page.cursor) return out;
      cursor = page.cursor;
    }
  }

  async agent(agentId: string): Promise<Agent> {
    return this.http.request("GET", `${this.base}/agents/${seg(agentId)}`);
  }

  /**
   * One page of deployed scripts and their active versions. Deploy with the CLI.
   *
   * PAGED, like `agents`. Both routes return a cursor and both used to have it dropped here, so a
   * caller past the first page saw part of an answer and was told nothing — which is the exact
   * shape the `agents`/`allAgents` pair below already exists to avoid.
   */
  async scripts(
    query: { cursor?: string; limit?: number } = {}
  ): Promise<{ scripts: Script[]; cursor: string | null }> {
    return this.http.request("GET", `${this.base}/scripts`, { query });
  }

  /** Every deployed script, walking the pages for you. */
  async allScripts(): Promise<Script[]> {
    const out: Script[] = [];
    let cursor: string | undefined;
    for (;;) {
      const page = await this.scripts({ cursor });
      out.push(...page.scripts);
      if (!page.cursor) return out;
      cursor = page.cursor;
    }
  }

  /** One page of declared schedules. */
  async schedules(
    query: { cursor?: string; limit?: number } = {}
  ): Promise<{ schedules: Schedule[]; cursor: string | null }> {
    return this.http.request("GET", `${this.base}/schedules`, { query });
  }

  /** Every declared schedule, walking the pages for you. */
  async allSchedules(): Promise<Schedule[]> {
    const out: Schedule[] = [];
    let cursor: string | undefined;
    for (;;) {
      const page = await this.schedules({ cursor });
      out.push(...page.schedules);
      if (!page.cursor) return out;
      cursor = page.cursor;
    }
  }

  /**
   * Declare a schedule. The cloud owns WHAT should happen; the machine owns WHEN — it fires from
   * its own clock, so a nightly export still runs while your internet is down and only the run's
   * record is late.
   */
  async createSchedule(req: Partial<Schedule> & { script: string; cron: string }): Promise<Schedule> {
    const { schedule } = await this.http.request<{ schedule: Schedule }>("POST", `${this.base}/schedules`, {
      body: req,
      retry: false,
    });
    return schedule;
  }

  async deleteSchedule(scheduleId: string): Promise<void> {
    await this.http.request("DELETE", `${this.base}/schedules/${seg(scheduleId)}`, { retry: false });
  }

  // --- inbound data --------------------------------------------------------

  /**
   * Hand a value to whichever job is waiting for it — `job.waitForData(key)` on the other end.
   *
   * THE CASE THIS IS FOR: a script signs into a portal, the portal sends a one-time code to a
   * mailbox or a phone, and whatever receives that message calls this. Everything else the agent
   * does is outbound; this is the one way to tell a run something.
   *
   * ADDRESSED BY KEY, not by run id, because the thing that will deliver the code — an SMS
   * webhook, an inbound-mail parser — was configured months before tonight's run existed. That
   * puts a duty on the key: the value reaches every job executing on the instance, and only one
   * that asks for that key ever sees it, so `otp.dealer-42` is the shape to write when more than
   * one job can be in flight. `script` and `agentId` narrow it further.
   *
   * Throws when nothing is running. The usual failure is a race you cannot see — the code arrived
   * four seconds after the script gave up — and an integration needs to log that rather than read
   * a silent success.
   *
   * `delivered` counts what the MACHINES confirmed. A run in `undetermined` was written to and did
   * not acknowledge, which is not a failure: do not send a replacement value, because the job may
   * already have this one and a second one-time code invalidates the first.
   */
  async send(
    key: string,
    value: unknown,
    opts: { script?: string; agentId?: string } = {}
  ): Promise<Delivery> {
    return this.http.request("POST", `${this.base}/data/${seg(key)}`, {
      query: { script: opts.script, agent_id: opts.agentId },
      body: value,
      retry: false,
    });
  }

  /** The same, to ONE run by id — for when you were given the run id, typically because the
   *  script handed out `job.dataUrl(key)` when it triggered whatever produces the value.
   *
   *  `delivered: false` with `undetermined: true` is the written-but-unacknowledged case: leave it
   *  alone and let the job's own timeout decide. */
  async sendToRun(
    runId: string,
    key: string,
    value: unknown
  ): Promise<{ delivered: boolean; undetermined?: true; run: string; key: string; message?: string }> {
    return this.http.request("POST", `${this.base}/runs/${seg(runId)}/data/${seg(key)}`, {
      body: value,
      retry: false,
    });
  }

  // --- credentials ---------------------------------------------------------

  /** The NAMES of the credentials this fleet's scripts read as `env.NAME`. Values are write-only
   *  and never returned — a leaked key can replace one but not harvest it. */
  async env(): Promise<string[]> {
    const { env } = await this.http.request<{ env: string[] }>("GET", `${this.base}/env`);
    return env;
  }

  /**
   * Replace the credential map. Omitting a name deletes it; `null` for a name KEEPS what is
   * stored, which is the only way to rotate one without re-typing the others you cannot read.
   */
  async setEnv(env: Record<string, string | null>): Promise<string[]> {
    const res = await this.http.request<{ env: string[] }>("PUT", `${this.base}/env`, {
      body: { env },
      retry: false,
    });
    return res.env;
  }
}
