/** Wire types for the automation data plane — scripts run on your own machines. */

export type RunStatus = "queued" | "running" | "done" | "failed" | "canceled" | "abandoned";

export interface StartRunRequest {
  /** A deployed script's name. Deploy with the CLI: `altengine automation deploy`. */
  script: string;
  /** The run's input, reachable in the script as `job.params`. One deployed script becomes many
   *  jobs through it — per dealer, per date, per account. */
  params?: unknown;
  /** Pin the run to one machine. Omit to let any machine matching `labels` take it. */
  agent_id?: string;
  /** Target by label instead of by name — `["site-dallas", "has-quickbooks"]`. */
  labels?: string[];
  /** Override the script's own parallel/exclusive setting for this run. Exclusive (the default
   *  for anything touching the desktop) means the run has the machine to itself. */
  parallel?: boolean;
  /** POSTed once the run AND its files have landed, HMAC-signed with the instance secret. */
  webhook_url?: string;
  /** A wall clock, if this job genuinely has a deadline. Usually unset: what bounds a run is the
   *  instance's cost ceiling, not a timer. */
  timeout_ms?: number;
}

export interface Run {
  id: string;
  status: RunStatus;
  script: string;
  version: number;
  agent_id: string | null;
  params: unknown;
  labels: string[];
  parallel: boolean;
  timeout_ms: number;
  /** Seconds this run was authorised for, derived from the instance's cost ceiling. */
  max_run_seconds: number;
  parent_run_id: string | null;
  chain_depth: number;
  exit_code: number | null;
  error: string | null;
  /** Present only while `running` AND doing something other than running the script — today
   *  `"uploading"`, with `phase_detail` like `"2 files, 3.1 GB"`. This is how a large export
   *  still going up is told apart from a script that is stuck. */
  phase?: string | null;
  phase_detail?: string | null;
  cost_usd: number;
  queued_at: number;
  started_at: number | null;
  ended_at: number | null;
  webhook: { status: string | null; attempts: number; at: number | null } | null;
}

export interface Artifact {
  id: string;
  run_id: string;
  name: string;
  size_bytes: number;
  content_type: string | null;
  created_at: number;
  expires_at: number;
  /** A presigned download link, minted for THIS response and never stored. It expires on its own
   *  in a few minutes, so fetch what you need rather than saving the URL. */
  url?: string;
  url_expires_in?: number;
}

export interface RunWithArtifacts {
  run: Run;
  artifacts: Artifact[];
}

export interface LogPage {
  lines: { ts: number; level: string; msg: string }[];
  cursor: string | null;
  /** Where these lines came from. `"agent"` is a live tail of the last few hundred lines from the
   *  machine itself; `"artifact"` is the complete log the run uploaded when it finished. A tail
   *  that stops is not a script that stopped — this is how you tell. */
  source?: string;
  dropped?: number;
}

export interface Agent {
  id: string;
  name: string;
  hostname: string | null;
  os: string | null;
  agent_version: string | null;
  labels: string[];
  online: boolean;
  last_seen: number | null;
  /** A THIRD state, not a shade of offline. Offline is a machine that worked and stopped; this is
   *  a credential nobody redeemed, so `hostname`, `os` and `agent_version` are null until it
   *  first connects. Do not report one as a machine that has stopped working. */
  never_connected: boolean;
  enrolled_at: number;
  revoked_at: number | null;
}

export interface Script {
  name: string;
  version: number;
  parallel: boolean;
  updated_at: number;
}

export interface Schedule {
  id: string;
  script: string;
  cron: string;
  timezone: string;
  params: unknown;
  labels: string[];
  agent_id: string | null;
  enabled: boolean;
  catchup_policy: string;
}
