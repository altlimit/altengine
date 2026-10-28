/** Where a push task is delivered: exactly one of a function, a channel or a webhook. */
export type TaskTarget =
  | { function: string } // "<functions-instance>/<function>"
  | { channel: string } // "<channel-instance>/<channel>"
  | { webhook: string }; // an https URL, signed like every altengine webhook

export type TaskState = "scheduled" | "running" | "leased" | "done" | "failed" | "cancelled";

/** Where a task came from. */
export type TaskSource = "api" | "console" | "mcp" | "function" | "datastore" | "blob" | "mailbox" | "deadletter" | "rerun";

export interface RetryPolicy {
  /** Attempts before the task fails, the first included. 1–100, default 10. */
  maxAttempts: number;
  /** 1–86400, default 10. */
  minBackoffSeconds: number;
  /** 1–604800, default 3600. */
  maxBackoffSeconds: number;
  /** How many times the wait doubles before it grows linearly. 0–16, default 8. */
  maxDoublings: number;
  /** A task older than this fails without another attempt. 0 (no limit) – 2592000, default 604800. */
  maxAgeSeconds: number;
}

export interface QueueSettings {
  /** `push` delivers each task to its target; `pull` holds tasks for workers to lease. */
  mode: "push" | "pull";
  /** The default push target; a task's own target overrides it. Null on a pull queue. */
  target: TaskTarget | null;
  /** 0.01–100, default 10. */
  ratePerSecond: number;
  /** Deliveries in flight at once, 1–50, default 5. */
  maxConcurrent: number;
  retry: RetryPolicy;
  /** Another queue in the same instance that receives a copy of every task that fails. */
  deadLetterQueue: string | null;
  paused: boolean;
  /** Let the target steer its own retries with `Retry-After`, `x-task-retry` and
   *  `x-task-max-attempts`. Default true. */
  targetControl: boolean;
  /** How long a push delivery is waited for before the attempt counts as failed and is retried,
   *  15–840, default 600. The target is not stopped when it passes. */
  deadlineSeconds: number;
}

/** What a create or a change sends. Only the fields given change. */
export type QueueSettingsPatch = Partial<Omit<QueueSettings, "retry">> & { retry?: Partial<RetryPolicy> };

export interface QueueInfo {
  name: string;
  settings: QueueSettings;
  createdAt: number;
  updatedAt: number;
  deleting: boolean;
  purging: boolean;
  /** Tasks per state, each capped at 10,000; `capped` names the states that reached it. */
  counts: Record<TaskState, number>;
  capped: TaskState[];
}

export interface TasksInfo {
  name: string;
  /** How long a finished task's record — and so its name — is kept. */
  retentionDays: number;
  queues: QueueInfo[];
}

/** One task to add. */
export interface TaskInput<P = unknown> {
  /** Any JSON, up to 128 KB. Required: pass null for none. */
  payload: P;
  /** Makes the add idempotent: a name already used in the queue is refused while its record is kept. */
  name?: string;
  /** Seconds from now. Give this or `runAt`, not both; neither is now. */
  delay?: number;
  /** Epoch milliseconds, an ISO 8601 time, or a Date — at most 30 days ahead. */
  runAt?: number | string | Date;
  /** Overrides the queue's target (push queues only). */
  target?: TaskTarget;
  /** A small object passed to the target beside the payload, up to 4 KB. */
  params?: Record<string, unknown>;
  /** This task's retry policy; fields left out take the defaults. */
  retry?: Partial<RetryPolicy>;
  /** Overrides the queue's `deadlineSeconds` for this task, 15–840. */
  deadlineSeconds?: number;
}

export type AddResult =
  | { ok: true; id: string; name: string | null; state: TaskState; runAt: number }
  | { ok: false; code: "TASK_EXISTS"; message: string; id: string; name: string };

/** A task without its payload, params or result — what a listing returns. */
export interface TaskSummary {
  id: string;
  queue: string;
  name: string | null;
  state: TaskState;
  target: TaskTarget | null;
  attempts: number;
  maxAttempts: number;
  runAt: number;
  createdAt: number;
  startedAt: number | null;
  finishedAt: number | null;
  leaseUntil: number | null;
  lastStatus: string | null;
  lastError: string | null;
  source: TaskSource;
  /** Trigger-chain depth: 0 for a task a person or a key added. */
  depth: number;
  payloadBytes: number;
  deadLetterOf: string | null;
  /** Times the target deferred it (`x-task-retry: defer`); not counted as attempts. */
  defers: number;
  /** This task's own deadline in seconds; null means its queue's. */
  deadlineSeconds: number | null;
}

export interface TaskAttempt {
  n: number;
  startedAt: number;
  endedAt: number | null;
  status: string | null;
  error: string | null;
}

export interface TaskRecord<P = unknown> extends TaskSummary {
  payload: P;
  params: Record<string, unknown> | null;
  /** The target's answer, up to 16 KB. */
  result: string | null;
  retry: RetryPolicy | null;
  attemptLog: TaskAttempt[];
}

/** A leased pull task, and the lease id that acks, extends or releases it. */
export interface LeasedTask<P = unknown> extends TaskRecord<P> {
  leaseId: string;
}

export interface TaskPage {
  tasks: TaskSummary[];
  cursor: string | null;
}

export interface TaskListQuery {
  /** Omit for every state, newest first. Waiting work comes soonest first; finished work latest first. */
  state?: TaskState;
  cursor?: string;
  /** 1–200, default 50. */
  limit?: number;
  /** Only the task with this name. */
  name?: string;
}

export interface CancelResult {
  cancelled: string[];
  /** Tasks left alone: running or finished (`state`), or not found (`state: null`). */
  skipped: { id: string; state: TaskState | null }[];
}

export interface LeaseOptions {
  /** 1–100, default 1. */
  count?: number;
  /** 1–3600, default 300. */
  leaseSeconds?: number;
}

export interface ReleaseOptions {
  /** When it may be leased again, in seconds. Default 0. */
  delaySeconds?: number;
  /** Recorded on the attempt. */
  error?: string;
}

/** What a push target receives about the task it is handed. */
export interface TaskEnvelope {
  id: string;
  queue: string;
  name: string | null;
  attempt: number;
  maxAttempts: number;
  runAt: number;
  createdAt: number;
  source: TaskSource;
  depth: number;
  params: Record<string, unknown> | null;
  deadLetterOf: string | null;
  /** Epoch ms at which the queue stops waiting for this attempt and counts it as failed. The
   *  target is not stopped then; finish before it, or the work may run again. */
  deadlineAt: number;
}

/** The JSON body a function or webhook target receives. Handle it idempotently on `task.id`:
 *  delivery is at least once. */
export interface TaskDelivery<P = unknown> {
  task: TaskEnvelope;
  payload: P;
}
