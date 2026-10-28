import { Http, seg } from "../http.js";
import type {
  AddResult,
  CancelResult,
  LeasedTask,
  LeaseOptions,
  QueueInfo,
  QueueSettingsPatch,
  ReleaseOptions,
  TaskInput,
  TaskListQuery,
  TaskPage,
  TaskRecord,
  TaskSummary,
  TasksInfo,
} from "./types.js";

/** Response headers a push target may answer with to steer its own retries (when the queue's
 *  `targetControl` is on, the default). */
export const TASK_RETRY_HEADER = "x-task-retry";
export const TASK_RETRY_AFTER_HEADER = "x-task-retry-after";
export const TASK_MAX_ATTEMPTS_HEADER = "x-task-max-attempts";

function wireTask(t: TaskInput): Record<string, unknown> {
  const { runAt, ...rest } = t;
  return runAt instanceof Date ? { ...rest, runAt: runAt.getTime() } : runAt === undefined ? rest : { ...rest, runAt };
}

/**
 * Server-side client for one tasks instance — queues of work.
 *
 * A push queue delivers each task to a function, a channel or a signed webhook and retries it
 * with backoff until it is accepted; a pull queue holds tasks for workers to `lease` and `ack`.
 * Delivery is at least once: handle a task idempotently on its id.
 *
 * ORGANIZATION API KEY ONLY: a task runs its target with the target's own grants, so end-user
 * identity tokens are refused.
 */
export class TasksClient {
  readonly instance: string;
  private readonly http: Http;
  private readonly base: string;

  constructor(http: Http, instance: string) {
    this.http = http;
    this.instance = instance;
    this.base = `/v1/tasks/${seg(instance)}`;
  }

  /** The instance's retention and its queues. */
  async info(): Promise<TasksInfo> {
    return this.http.request("GET", this.base);
  }

  // --- queues ---------------------------------------------------------------

  /** Every queue, with its settings and counts by state. */
  async queues(): Promise<QueueInfo[]> {
    const { queues } = await this.http.request<{ queues: QueueInfo[] }>("GET", `${this.base}/queues`);
    return queues;
  }

  async queue(name: string): Promise<QueueInfo> {
    const { queue } = await this.http.request<{ queue: QueueInfo }>("GET", `${this.base}/queues/${seg(name)}`);
    return queue;
  }

  /** Create a queue. Settings left out take their defaults. Needs `full`. */
  async createQueue(name: string, settings: QueueSettingsPatch = {}): Promise<QueueInfo> {
    const { queue } = await this.http.request<{ queue: QueueInfo }>("POST", `${this.base}/queues`, {
      body: { ...settings, name },
    });
    return queue;
  }

  /** Change a queue: only the settings given change. Needs `full`. */
  async updateQueue(name: string, settings: QueueSettingsPatch): Promise<QueueInfo> {
    const { queue } = await this.http.request<{ queue: QueueInfo }>("PATCH", `${this.base}/queues/${seg(name)}`, {
      body: settings,
    });
    return queue;
  }

  /** Stop delivering a queue's tasks; nothing is lost. Needs `full`. */
  async pauseQueue(name: string): Promise<QueueInfo> {
    const { queue } = await this.http.request<{ queue: QueueInfo }>("POST", `${this.base}/queues/${seg(name)}/pause`);
    return queue;
  }

  async resumeQueue(name: string): Promise<QueueInfo> {
    const { queue } = await this.http.request<{ queue: QueueInfo }>("POST", `${this.base}/queues/${seg(name)}/resume`);
    return queue;
  }

  /** Delete every task in a queue, in any state. Needs `full`. */
  async purgeQueue(name: string): Promise<{ purged: number; pending: boolean }> {
    return this.http.request("POST", `${this.base}/queues/${seg(name)}/purge`);
  }

  /** Delete a queue and every task in it. `default` cannot be deleted. Needs `full`. */
  async deleteQueue(name: string): Promise<{ deleted: boolean; pending: boolean }> {
    return this.http.request("DELETE", `${this.base}/queues/${seg(name)}`);
  }

  // --- tasks ----------------------------------------------------------------

  /**
   * Add one task, or up to 100, to a queue. Each result says whether it was added or its name
   * was already taken — a name already used is a result, not a throw, so a producer that retries
   * with the same name is idempotent.
   *
   * Retried on a transient failure only when every task is named: an unnamed task sent twice is
   * two tasks.
   */
  async add<P = unknown>(queue: string, tasks: TaskInput<P> | TaskInput<P>[]): Promise<AddResult[]> {
    const list = Array.isArray(tasks) ? tasks : [tasks];
    const res = await this.http.request<{ tasks: AddResult[] }>("POST", `${this.base}/queues/${seg(queue)}/tasks`, {
      body: { tasks: list.map(wireTask) },
      retry: list.every((t) => !!t.name) ? undefined : false,
      // Every name already taken is a 409 whose body is still the per-task answer.
      acceptStatus: [409],
    });
    return res.tasks;
  }

  /** One page of a queue's tasks, without payloads. */
  async list(queue: string, query: TaskListQuery = {}): Promise<TaskPage> {
    return this.http.request("GET", `${this.base}/queues/${seg(queue)}/tasks`, { query: { ...query } });
  }

  /** Every task in a queue (narrowed by `query`), page by page. */
  async *listAll(queue: string, query: Omit<TaskListQuery, "cursor"> = {}): AsyncGenerator<TaskSummary> {
    let cursor: string | undefined;
    for (;;) {
      const page = await this.list(queue, { ...query, cursor });
      for (const t of page.tasks) yield t;
      if (!page.cursor) return;
      cursor = page.cursor;
    }
  }

  /** One task, with its payload, every attempt and the target's answer. */
  async get<P = unknown>(id: string): Promise<TaskRecord<P>> {
    const { task } = await this.http.request<{ task: TaskRecord<P> }>("GET", `${this.base}/tasks/${seg(id)}`);
    return task;
  }

  /** Cancel scheduled or leased tasks (up to 200). A running task cannot be stopped. */
  async cancel(ids: string | string[]): Promise<CancelResult> {
    return this.http.request("POST", `${this.base}/tasks/cancel`, { body: { ids: Array.isArray(ids) ? ids : [ids] } });
  }

  /**
   * Run a task now: a scheduled one is moved up, a failed or cancelled one is retried with its
   * attempts reset, and a finished one is run again as a NEW task with the same payload.
   */
  async run<P = unknown>(id: string): Promise<TaskRecord<P>> {
    const { task } = await this.http.request<{ task: TaskRecord<P> }>("POST", `${this.base}/tasks/${seg(id)}/run`, { retry: false });
    return task;
  }

  // --- pull queues ----------------------------------------------------------

  /**
   * Lease up to `count` due tasks from a pull queue. Finish each with `ack`, or give it back with
   * `release`; a lease that runs out puts the task back with its attempt counted as failed.
   * Never retried: a retried lease would hold tasks nobody is working on.
   */
  async lease<P = unknown>(queue: string, opts: LeaseOptions = {}): Promise<LeasedTask<P>[]> {
    const { tasks } = await this.http.request<{ tasks: LeasedTask<P>[] }>("POST", `${this.base}/queues/${seg(queue)}/lease`, {
      body: opts,
      retry: false,
    });
    return tasks;
  }

  /** Finish a leased task, with an optional result (a string, or JSON) kept up to 16 KB. */
  async ack<P = unknown>(id: string, leaseId: string, result?: unknown): Promise<TaskRecord<P>> {
    const { task } = await this.http.request<{ task: TaskRecord<P> }>("POST", `${this.base}/tasks/${seg(id)}/ack`, {
      body: { leaseId, result: result ?? null },
    });
    return task;
  }

  /** Hold a lease longer: `leaseSeconds` (1–3600, default 300) from now. */
  async extend<P = unknown>(id: string, leaseId: string, leaseSeconds?: number): Promise<TaskRecord<P>> {
    const { task } = await this.http.request<{ task: TaskRecord<P> }>("POST", `${this.base}/tasks/${seg(id)}/extend`, {
      body: { leaseId, leaseSeconds },
    });
    return task;
  }

  /** Give a leased task back, to be leased again after `delaySeconds`. The attempt counts as
   *  failed; on the last attempt the task fails instead. */
  async release<P = unknown>(id: string, leaseId: string, opts: ReleaseOptions = {}): Promise<TaskRecord<P>> {
    const { task } = await this.http.request<{ task: TaskRecord<P> }>("POST", `${this.base}/tasks/${seg(id)}/release`, {
      body: { leaseId, ...opts },
    });
    return task;
  }
}
