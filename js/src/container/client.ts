import { Http, seg } from "../http.js";
import { sleep } from "../poll.js";
import type {
  ContainerSizes,
  Job,
  JobListQuery,
  JobLogPage,
  JobPage,
  LaunchRequest,
} from "./types.js";

/** How long `wait` sleeps between polls, and the ceiling it backs off to. A job runs for
 *  minutes or hours, so polling fast buys nothing and costs a request every time. */
const POLL_START_MS = 2_000;
const POLL_MAX_MS = 15_000;

const TERMINAL = new Set<string>(["done", "failed", "canceled"]);

/**
 * Server-side client for one container instance — a Docker image run as a background job, for
 * the work that will not fit in a function.
 *
 * ORGANIZATION API KEY ONLY. End-user identity tokens are refused by this service, as they are
 * by automation: access rules bound what a user may READ, not what they may SPEND. Have a page
 * call a function that decides.
 *
 * A job's OUTPUT does not come back through here. It writes what it produces to blob — name a
 * `blobStore` on the instance and every job gets `AE_BLOB_URL` and `AE_BLOB_TOKEN` — or it calls
 * a function when it finishes.
 */
export class ContainerClient {
  readonly instance: string;
  private readonly http: Http;
  private readonly base: string;

  constructor(http: Http, instance: string) {
    this.http = http;
    this.instance = instance;
    this.base = `/v1/container/${seg(instance)}`;
  }

  /**
   * Start a job and return as soon as its machine exists.
   *
   * There is nothing to await: a job that runs for twenty minutes has nowhere to send twenty
   * minutes of output. Poll with `wait`, or have the instance call a function when it ends.
   */
  async run(req: LaunchRequest): Promise<Job> {
    const { job } = await this.http.request<{ job: Job }>("POST", this.base, {
      body: req,
      // A retried launch is a SECOND MACHINE, billed for its own runtime.
      retry: false,
    });
    return job;
  }

  /** One job: status, exit code, how long it ran, what it cost. */
  async get(jobId: string): Promise<Job> {
    const { job } = await this.http.request<{ job: Job }>("GET", `${this.base}/${seg(jobId)}`);
    return job;
  }

  /**
   * One page of jobs, newest first.
   *
   * PAGED, and the cursor matters. Note the shape: the response's `cursor` is fed back as
   * `before` — it is the `started_at` of the last job on the page.
   */
  async list(query: JobListQuery = {}): Promise<JobPage> {
    return this.http.request("GET", this.base, { query });
  }

  /** Every job matching the query, walking the pages for you. */
  async *listAll(query: Omit<JobListQuery, "before"> = {}): AsyncGenerator<Job> {
    let before: number | undefined;
    for (;;) {
      const page = await this.list({ ...query, before });
      for (const j of page.jobs) yield j;
      if (page.cursor == null) return;
      before = page.cursor;
    }
  }

  /**
   * Stop a running job and destroy its machine now. It is billed for the time it ran.
   *
   * `canceled: false` means the job had already finished, which is not a failure.
   */
  async cancel(jobId: string): Promise<{ job: Job; canceled: boolean }> {
    return this.http.request("POST", `${this.base}/${seg(jobId)}/cancel`, { retry: false });
  }

  /**
   * What the job printed, oldest first.
   *
   * BEST EFFORT: an unavailable log service returns an empty page with a `note` rather than
   * throwing, and a job's own record — exit code, timing, cost — never depends on it. Logs are
   * kept for about a week after the machine is destroyed.
   */
  async logs(jobId: string, query: { cursor?: string | number } = {}): Promise<JobLogPage> {
    return this.http.request("GET", `${this.base}/${seg(jobId)}/logs`, { query });
  }

  /** What this instance will accept: the sizes, the image allowlist, and the two ceilings. */
  async sizes(): Promise<ContainerSizes> {
    return this.http.request("GET", `${this.base}/sizes`);
  }

  /**
   * Poll until a job reaches a terminal status.
   *
   * `timeoutMs` bounds the WAIT, not the job: giving up here leaves the machine running and
   * billing. Call `cancel` if that is what you meant. Zero (the default) waits indefinitely.
   */
  async wait(
    jobId: string,
    opts: { timeoutMs?: number; signal?: AbortSignal; onPoll?: (job: Job) => void } = {}
  ): Promise<Job> {
    const deadline = opts.timeoutMs ? Date.now() + opts.timeoutMs : 0;
    let delay = POLL_START_MS;
    for (;;) {
      const job = await this.get(jobId);
      opts.onPoll?.(job);
      if (TERMINAL.has(job.status)) return job;
      if (deadline && Date.now() >= deadline) {
        throw new Error(
          `job ${jobId} was still ${job.status} after ${opts.timeoutMs}ms — its machine is still running and still billing; cancel it if that is not what you want`
        );
      }
      let ms = delay;
      if (deadline) ms = Math.min(ms, Math.max(0, deadline - Date.now()));
      await sleep(ms, opts.signal);
      delay = Math.min(delay * 2, POLL_MAX_MS);
    }
  }

  /** Start a job and wait for it. Convenience over `run` + `wait`. */
  async runAndWait(
    req: LaunchRequest,
    opts: { timeoutMs?: number; signal?: AbortSignal; onPoll?: (job: Job) => void } = {}
  ): Promise<Job> {
    const started = await this.run(req);
    return this.wait(started.id, opts);
  }
}
