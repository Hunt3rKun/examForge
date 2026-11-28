import { useState } from "react";

export function App(): JSX.Element {
  const [selected, setSelected] = useState("catalog");
  return <main className="min-h-screen bg-slate-50">
    <header className="border-b border-slate-200 bg-white px-8 py-4">
      <div className="mx-auto flex max-w-[1500px] items-center justify-between">
        <div><p className="text-xs font-semibold uppercase tracking-[0.2em] text-indigo-600">ExamForge</p><h1 className="text-xl font-semibold text-slate-900">Review workbench</h1></div>
        <nav className="flex gap-2" aria-label="Primary navigation">
          {[["catalog", "Catalog"], ["workbench", "Workbench"]].map(([id, label]) => <button key={id} className={`rounded-md px-3 py-2 text-sm ${selected === id ? "bg-indigo-50 text-indigo-700" : "text-slate-600 hover:bg-slate-100"}`} onClick={() => setSelected(id)}>{label}</button>)}
        </nav>
      </div>
    </header>
    <section className="mx-auto max-w-[1500px] px-8 py-10"><div className="rounded-xl border border-dashed border-slate-300 bg-white p-12 text-center"><h2 className="text-2xl font-semibold text-slate-900">{selected === "catalog" ? "Choose an exam to begin" : "Workbench components coming soon"}</h2><p className="mx-auto mt-3 max-w-xl text-slate-500">The web core is ready for API-backed catalog, source PDF, and structured question editor features.</p></div></section>
  </main>;
}
