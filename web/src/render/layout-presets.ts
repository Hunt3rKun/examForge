import { layoutSpec } from "../domain/layout";
const studentA4 = layoutSpec({name: "Student A4", paper_size: "a4", columns: 1, mode: "student", show_answer: false, show_analysis: false, show_binding_line: false, option_layout: "auto"});
export const layoutPresets = { studentA4: Object.freeze(studentA4), teacherA4: Object.freeze(layoutSpec({...studentA4, name: "Teacher A4", mode: "teacher", show_answer: true, show_analysis: true})), a3TwoColumns: Object.freeze(layoutSpec({...studentA4, name: "A3 two columns", paper_size: "a3", columns: 2, show_binding_line: true})) };
