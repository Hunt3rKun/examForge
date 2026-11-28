import {buildItemPatch, createDraft, editDraft, lossyFields} from "../domain/item-patch";
import type {QuestionItem} from "../domain/exam";
const text = (markdown: string) => ({type: "text", markdown});
const child: QuestionItem = {kind:"single",display_number:"1a",stem:[text("Child")]};
const parent: QuestionItem = {kind:"composite",display_number:"1",stem:[text("Parent")], score: 2, answer:{short_values:["x"]}, children:[child], provenance:{review_status:"pending",bboxes:[{page:1,rect:[0,0,1,1]}]}};
const base = () => createDraft(parent, '"item-i-r1"');

it("only sends dirty keys including clear values and never sends children or provenance fields other than review status", () => {
  let next = editDraft(base(), "stem", [text("Changed")]);
  next = editDraft(next, "answer", null);
  next = editDraft(next, "score", null);
  next = editDraft(next, "options", []);
  next = editDraft(next, "review_status", "");
  expect(buildItemPatch(next)).toEqual({patch:{stem:[text("Changed")],answer:null,score:null,options:[],provenance:{review_status:""}},blockedFields:[]});
  expect(JSON.stringify(next.draft.children)).toBe(JSON.stringify(parent.children));
  expect(next.draft.children).not.toBe(parent.children);
  expect(next.draft.provenance?.bboxes).toEqual(parent.provenance?.bboxes);
  next = editDraft(next,"stem",parent.stem);
  expect(next.dirtyFields.has("stem")).toBe(false);
});

it("does not consume replacement ETags or unlock a conflicted draft on edit", () => {
  const draft = {...editDraft(base(), "score", 4),saveState:"conflict" as const};
  const next = editDraft(draft,"score",5);
  expect(next).toMatchObject({originalEtag:'"item-i-r1"',saveState:"conflict"});
  expect(buildItemPatch(next).patch).toEqual({score:5});
});

it("blocks replacing unknown blocks but preserves safe siblings and known read-only tables", () => {
  const extension = {type:"text",markdown:"x",extra:"cannot be round-tripped"};
  const table = {type:"table",table:{headers:[{text:"h"}],rows:[[{text:"c"}]]}};
  let draft = createDraft({...parent,stem:[text("before"),table],answer:{detail:[extension]}}, '"item-i-r1"');
  expect(lossyFields(draft.original)).toEqual(["answer"]);
  draft = editDraft(draft,"answer",null);
  draft = editDraft(draft,"stem",[text("after"),table]);
  expect(buildItemPatch(draft)).toEqual({patch:{stem:[text("after"),table]},blockedFields:["answer"]});
  draft = editDraft(draft,"stem",[text("after")]);
  expect(buildItemPatch(draft)).toEqual({patch:{},blockedFields:["stem","answer"]});
});

it("blocks wrapper or table extensions even if the draft removes them", () => {
  const candidate = {kind:"single",display_number:"1",stem:[{type:"table",table:{headers:[{text:"head",alien:1}],rows:[]}}],options:[{label:"A",content:[],alien:1}],metadata:{difficulty:0.5,alien:1}} satisfies Omit<QuestionItem,"stem"|"options"|"metadata"> & Record<string, unknown>;
  expect(lossyFields(candidate)).toEqual(["stem","options","metadata"]);
});

it("rejects invalid numeric values and null required fields without mutating state", () => {
  const draft = base();
  expect(editDraft(draft,"score",Number.NaN)).toBe(draft);
  expect(editDraft(draft,"score",1_000_001)).toBe(draft);
  expect(editDraft(draft,"stem",null)).toBe(draft);
  expect(editDraft(draft,"review_status","not a status")).toBe(draft);
});
