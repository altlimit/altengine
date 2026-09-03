/** Wire types for the container data plane — a Docker image run as a background job. */

export type JobStatus = "running" | "done" | "failed" | "canceled";

export interface LaunchRequest {
  /** Must be on the instance's allowed-images list. An empty allowlist runs nothing. */
  image: string;
  /** `small`, `medium` or `large` — ask `sizes()` what this instance offers. */
  size?: string;
  /** Overrides the image's own entrypoint arguments. */
  cmd?: string[];
  /** Variables for the job. Names starting `AE_` are reserved. */
  env?: Record<string, string>;
  /** The job's deadline. Bounded by the instance's own ceiling, and by what one job may cost. */
  timeout_ms?: number;
}

export interface Job {
  id: string;
  status: JobStatus;
  image: string;
  size: string;
  exit_code: number | null;
  timeout_ms: number;
  error: string | null;
  started_at: number;
  ended_at: number | null;
  cost_usd: number;
}

export interface JobPage {
  jobs: Job[];
  /** The `started_at` to feed back as `before`. Null when there is nothing older. */
  cursor: number | null;
}

/** A type alias rather than an interface so it satisfies the query-string record. */
export type JobListQuery = {
  status?: JobStatus;
  limit?: number;
  /** Keyset cursor: the `cursor` from the previous page. */
  before?: number;
};

export interface JobLogLine {
  timestamp: string;
  /** The stream the line came from. */
  level?: string;
  message: string;
}

export interface JobLogPage {
  lines: JobLogLine[];
  cursor: string | number | null;
  /** Why a page is empty, when it is empty for a reason worth showing — logs that have aged
   *  out, a job that never had a machine, a log service that could not be reached. */
  note?: string;
}

export interface ContainerSizes {
  sizes: { name: string; cpus: number; memory_mb: number }[];
  /** Nothing outside this list will launch. */
  allowed_images: string[];
  max_timeout_ms: number;
  max_concurrent: number;
}
