import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createApiClient } from "../api/client";
import { CatalogPage } from "../features/catalog";
import { Workbench } from "../features/workbench/Workbench";

const confirmLeave = () => window.confirm("离开校对台并放弃未保存的修改？正在进行的写入可能已提交。");

function examAt(path: string): string | undefined {
  const match = /^\/exams\/([^/]+)\/?$/.exec(path);
  if (!match) return undefined;
  try { return decodeURIComponent(match[1]); } catch { return undefined; }
}

export function App() {
  const baseUrl = import.meta.env.VITE_API_BASE_URL ?? "";
  const api = useMemo(() => createApiClient({ baseUrl }), [baseUrl]);
  const catalogApi = useMemo(() => ({
    listJobs: ({signal, ...query}: Parameters<import("../features/catalog").CatalogApi["listJobs"]>[0]) => api.listJobs(query, {signal}),
    listExams: ({signal, ...query}: Parameters<import("../features/catalog").CatalogApi["listExams"]>[0]) => api.listExams(query, {signal}),
    getJob: (id: string, signal: AbortSignal) => api.getJob(id, {signal}),
    submitIngest: (file: File, signal: AbortSignal) => api.submitIngest(file, {signal}),
  }), [api]);
  const [path, setPath] = useState(() => window.location.pathname);
  const current = useRef(path);
  const dirty = useRef(false);
  const updateDirty = useCallback((value: boolean) => { dirty.current = value; }, []);

  const navigate = useCallback((next: string) => {
    if (next === current.current || (dirty.current && !confirmLeave())) return;
    window.history.pushState(null, "", next);
    current.current = next;
    setPath(next);
  }, []);

  useEffect(() => {
    function onPop() {
      const next = window.location.pathname;
      if (next === current.current) return;
      if (dirty.current && !confirmLeave()) {
        window.history.pushState(null, "", current.current);
        return;
      }
      current.current = next;
      setPath(next);
    }
    const onExit = (event: BeforeUnloadEvent) => {
      if (!dirty.current) return;
      event.preventDefault();
      event.returnValue = true;
    };
    window.addEventListener("popstate", onPop);
    window.addEventListener("beforeunload", onExit);
    return () => {
      window.removeEventListener("popstate", onPop);
      window.removeEventListener("beforeunload", onExit);
    };
  }, []);

  const examId = examAt(path);
  if (examId) return <Workbench key={examId} api={api} apiBaseUrl={baseUrl} examId={examId} onHome={() => navigate("/")} onDirtyChange={updateDirty} />;
  return <>
    <header className="border-b border-slate-200 bg-white px-8 py-4">
      <div className="mx-auto max-w-[1500px]">
        <p className="text-xs font-semibold uppercase tracking-[0.2em] text-indigo-700">ExamForge · 铸卷</p>
        <h1 className="mt-1 text-xl font-semibold text-slate-900">试卷校对工作台</h1>
      </div>
    </header>
    <CatalogPage api={catalogApi} onOpenExam={(id) => navigate(`/exams/${encodeURIComponent(id)}`)} />
  </>;
}
