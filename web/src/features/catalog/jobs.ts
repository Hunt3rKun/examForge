import { useEffect, useState } from "react";
import type { components } from "../../api/schema.gen";
import { diagnosed, requestFailure, type CatalogResponse } from "../ingest/upload";

export type Job = components["schemas"]["Job"];
export type JobState = components["schemas"]["JobState"];
export type JobFilter = JobState | "all";
export interface JobsApi {
  listJobs(input: { limit: number; cursor?: string; state?: JobState; signal: AbortSignal }): Promise<CatalogResponse<components["schemas"]["JobPage"]>>;
  getJob(jobId: string, signal: AbortSignal): Promise<CatalogResponse<Job>>;
}

type ReadError = { message: string; limited: boolean };
type JobRead = { response?: CatalogResponse<Job>; error?: ReadError };

const isActiveJob = (job: Job) => job.state !== "persisted" && job.state !== "failed";
export function jobStatusLabel(job: Job): string {
  if (job.state === "failed") return job.error_code ? `Failed · ${job.error_code}` : "Failed";
  if (job.state === "persisted") return "Ready to review";
  return `${job.state[0].toUpperCase()}${job.state.slice(1)}`;
}

export function useJobDetails(api: JobsApi, listed: readonly Job[], uploaded: readonly Job[]) {
  const [reads, setReads] = useState<Record<string, JobRead>>({});
  const byId = new Map(listed.map((job) => [job.job_id, job]));
  for (const job of uploaded) if (!byId.has(job.job_id)) byId.set(job.job_id, job);
  const urgent = new Set<string>();
  for (const job of uploaded) if (!reads[job.job_id]) urgent.add(job.job_id);

  const effective = new Map<string, { job: Job; requestId?: string | null; readError?: string }>();
  for (const [id, job] of byId) {
    const read = reads[id];
    // Detail is authoritative for a matching or older list snapshot. Polling
    // does not rank states: River can move validating back to parsing.
    const detail = read?.response && Date.parse(read.response.data.updated_at) >= Date.parse(job.updated_at) ? read.response : undefined;
    if (!detail && !read?.error && job.state === "failed") urgent.add(id);
    effective.set(id, { job: detail?.data ?? job, requestId: detail?.requestId, readError: read?.error?.message });
  }
  const urgentKey = JSON.stringify([...urgent].sort());
  const activeKey = JSON.stringify([...effective.values()]
    .filter(({ job }) => isActiveJob(job) && !reads[job.job_id]?.error?.limited)
    .map(({ job }) => job.job_id).sort());

  useEffect(() => {
    const first = JSON.parse(urgentKey) as string[];
    const active = JSON.parse(activeKey) as string[];
    if (!first.length && !active.length) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let running: AbortController | undefined;
    let disposed = false;
    let pending = first;

    function stop() {
      clearTimeout(timer);
      timer = undefined;
      const request = running;
      running = undefined;
      request?.abort();
    }

    function schedule() {
      if (disposed || document.hidden || running || timer !== undefined) return;
      if (pending.length) void read(pending);
      else if (active.length) timer = setTimeout(() => {
        timer = undefined;
        void read(active);
      }, 2500);
    }

    async function read(ids: string[]) {
      const request = new AbortController();
      running = request;
      const entries: [string, JobRead][] = [];
      let next = 0;
      async function worker() {
        while (!request.signal.aborted && next < ids.length) {
          const id = ids[next++];
          try {
            const response = await api.getJob(id, request.signal);
            entries.push([id, { response }]);
          } catch (error) {
            const { status, requestId } = requestFailure(error);
            const limited = status === 429;
            const message = limited ? "The server is limiting requests. Refresh jobs later to resume checking this job." : "Could not confirm the latest job status.";
            entries.push([id, { error: { limited, message: diagnosed(message, requestId) } }]);
          }
        }
      }
      await Promise.all(Array.from({ length: Math.min(ids.length, 4) }, worker));
      if (running !== request || request.signal.aborted) return;
      running = undefined;
      pending = [];
      setReads((old) => {
        const updated = { ...old };
        for (const [id, result] of entries) {
          // A failed poll must not replace a previously authoritative state.
          updated[id] = result.error ? { ...updated[id], error: result.error } : result;
        }
        return updated;
      });
      schedule();
    }

    function visibilityChanged() {
      if (document.hidden) stop();
      else schedule();
    }
    document.addEventListener("visibilitychange", visibilityChanged);
    schedule();
    return () => {
      disposed = true;
      stop();
      document.removeEventListener("visibilitychange", visibilityChanged);
    };
  }, [api, activeKey, urgentKey]);

  function retryReadErrors() {
    setReads((old) => Object.fromEntries(Object.entries(old)
      .flatMap(([id, read]) => read.error && !read.response ? [] : [[id, { response: read.response }]])));
  }
  return { effective, retryReadErrors };
}
