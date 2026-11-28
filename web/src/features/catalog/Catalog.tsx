import { useCallback, useState, type MouseEvent, type ReactNode } from "react";
import type { components } from "../../api/schema.gen";
import { UploadPanel } from "../ingest/UploadPanel";
import { type CatalogResponse, type IngestAccepted, type IngestApi } from "../ingest/upload";
import { jobStatusLabel, useJobDetails, type Job, type JobFilter, type JobsApi } from "./jobs";
import { useCursorPages, type PageRequest } from "./pagination";

type Exam = components["schemas"]["ExamSummary"];
export interface CatalogApi extends JobsApi, IngestApi {
  listExams(input: PageRequest): Promise<CatalogResponse<components["schemas"]["ExamPage"]>>;
}
export type CatalogProps = { api: CatalogApi; onOpenExam?: (examId: string) => void };
const filters: ReadonlyArray<[JobFilter, string]> = [["all", "All"], ["queued", "Queued"], ["parsing", "Parsing"], ["validating", "Validating"], ["persisted", "Persisted"], ["failed", "Failed"]];
const idOfJob = (job: Job) => job.job_id;
const idOfExam = (exam: Exam) => exam.exam_id;
const card = "min-w-0 rounded-xl border border-slate-200 bg-white p-5 shadow-sm";
const action = "text-sm font-medium text-indigo-700 underline disabled:opacity-50";

function openExam(event: MouseEvent<HTMLAnchorElement>, id: string, navigate?: CatalogProps["onOpenExam"]) {
  if (!navigate || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  event.preventDefault();
  navigate(id);
}

function DateTime({ value }: { value: string }) {
  return <time dateTime={value}>{new Date(value).toLocaleString()}</time>;
}

function ListFooter({ page, label, children }: {
  page: { busy: boolean; next: string | null; error?: string; more: () => void };
  label: string;
  children: ReactNode;
}) {
  return <>
    {page.busy && <p role="status" className="mt-3 text-sm text-slate-600">Loading {label}…</p>}
    {page.error && <p role="alert" className="mt-3 text-sm text-rose-700">{page.error}</p>}
    {!page.busy && !page.error && children}
    {page.next !== null && <button type="button" className={`${action} mt-3`} disabled={page.busy} onClick={page.more}>Load more {label}</button>}
  </>;
}

export function CatalogPage({ api, onOpenExam }: CatalogProps) {
  const [filter, setFilter] = useState<JobFilter>("all");
  const [uploaded, setUploaded] = useState<Job[]>([]);
  const loadJobs = useCallback((input: PageRequest) => api.listJobs({ ...input, state: filter === "all" ? undefined : filter }), [api, filter]);
  const loadExams = useCallback((input: PageRequest) => api.listExams(input), [api]);
  const jobs = useCursorPages(loadJobs, idOfJob, "jobs");
  const exams = useCursorPages(loadExams, idOfExam, "exams");
  const { effective: details, retryReadErrors } = useJobDetails(api, jobs.items, uploaded);
  // Newly submitted jobs lead the catalog even if the current page request had
  // already started when the upload was accepted.
  const visible = [...new Set([...uploaded, ...jobs.items].map(idOfJob))]
    .map((id) => details.get(id)!)
    .filter(({ job }) => filter === "all" || job.state === filter);

  function accepted({ data }: CatalogResponse<IngestAccepted>) {
    setFilter("all");
    setUploaded((items) => [{ ...data, attempt_count: 0 }, ...items.filter((item) => item.job_id !== data.job_id)]);
  }

  function refreshJobs() {
    retryReadErrors();
    jobs.refresh();
  }

  function refreshAfterAmbiguousUpload() {
    if (filter === "all") refreshJobs();
    else setFilter("all");
  }

  return <div className="mx-auto grid max-w-[1500px] gap-6 px-8 py-8 lg:grid-cols-[minmax(350px,1fr)_minmax(440px,1.25fr)]">
    <div className="min-w-0 space-y-6">
      <UploadPanel api={api} onJobAccepted={accepted} onRefreshJobs={refreshAfterAmbiguousUpload} />
      <section aria-labelledby="jobs-heading" className={card}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h2 id="jobs-heading" className="text-lg font-semibold">Jobs</h2>
          <button type="button" className={action} onClick={refreshJobs}>Refresh jobs</button>
        </div>
        <label className="mt-3 flex items-center gap-2 text-sm">
          Job state
          <select className="rounded-md border border-slate-300 px-2 py-1" value={filter} onChange={(event) => setFilter(event.currentTarget.value as JobFilter)}>
            {filters.map(([key, label]) => <option key={key} value={key}>{label}</option>)}
          </select>
        </label>
        <ul className="mt-3 divide-y divide-slate-100">
          {visible.map(({ job, requestId, readError }) => <li key={job.job_id} className="py-3">
            <p className="break-all font-mono text-xs text-slate-500">{job.job_id}</p>
            <p className="text-sm font-medium">{jobStatusLabel(job)}</p>
            <p className="mt-1 text-xs text-slate-600">Updated <DateTime value={job.updated_at} /> · {job.attempt_count} attempts</p>
            {job.state === "failed" && <div className="mt-1 break-words text-sm text-rose-700">
              <p>{job.error_summary || "No error summary was provided."}</p>
              {requestId && <p>Request ID: {requestId}</p>}
            </div>}
            {readError && <p className="mt-1 text-xs text-slate-600">{readError}</p>}
            {job.state === "persisted" && job.exam_id && <a className={action} href={`/exams/${encodeURIComponent(job.exam_id)}`} onClick={(event) => openExam(event, job.exam_id!, onOpenExam)}>Open exam</a>}
          </li>)}
        </ul>
        <ListFooter page={jobs} label="jobs">{!visible.length && <p className="py-3 text-sm text-slate-600">No jobs in this filter.</p>}</ListFooter>
      </section>
    </div>
    <section aria-labelledby="exams-heading" className={card}>
      <div className="flex items-center justify-between gap-3">
        <h2 id="exams-heading" className="text-lg font-semibold">Exams</h2>
        <button type="button" className={action} onClick={exams.refresh}>Refresh exams</button>
      </div>
      <ul className="mt-3 divide-y divide-slate-100">
        {exams.items.map((exam) => <li key={exam.exam_id} className="py-4">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <a className="break-words font-medium text-indigo-700 underline" href={`/exams/${encodeURIComponent(exam.exam_id)}`} onClick={(event) => openExam(event, exam.exam_id, onOpenExam)}>{exam.title || "Untitled exam"}</a>
            <span className="rounded-full bg-amber-50 px-2 py-1 text-xs font-semibold text-amber-800">{exam.pending_review_count} pending review</span>
          </div>
          <p className="mt-1 text-sm text-slate-600">{exam.subject || "No subject"} · {exam.item_count ?? 0} items</p>
          <p className="mt-1 text-xs text-slate-600"><DateTime value={exam.created_at} /></p>
        </li>)}
      </ul>
      <ListFooter page={exams} label="exams">{!exams.items.length && <p className="py-3 text-sm text-slate-600">No exams yet. Upload a source PDF to start a job.</p>}</ListFooter>
    </section>
  </div>;
}
