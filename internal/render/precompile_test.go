package render

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"examforge/internal/ir"
	"examforge/internal/ir/inline"
)

// truthLayout is one row of the spec §9.2 truth table.
type truthRow struct {
	mode         string
	showAnswer   bool
	showAnalysis bool
}

var truthTable = []truthRow{
	{ModeStudent, false, false},
	{ModeStudent, false, true},
	{ModeStudent, true, false},
	{ModeStudent, true, true},
	{ModeTeacher, false, false},
	{ModeTeacher, false, true},
	{ModeTeacher, true, false},
	{ModeTeacher, true, true},
}

func layoutFor(row truthRow) ir.LayoutSpec {
	return ir.LayoutSpec{
		SchemaVersion: "1.0.0", Name: "truth", PaperSize: "a4", Columns: 1,
		Mode: row.mode, ShowAnswer: row.showAnswer, ShowAnalysis: row.showAnalysis,
		ShowBindingLine: false, OptionLayout: OptionAuto,
	}
}

// tokenTypes flattens a token list into its type sequence for assertions.
func tokenTypes(tokens []inline.Token) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, t.Type)
	}
	return out
}

// findToken returns the first token of a type, or nil.
func findToken(tokens []inline.Token, typ string) *inline.Token {
	for i := range tokens {
		if tokens[i].Type == typ {
			return &tokens[i]
		}
	}
	return nil
}

// TestPrecompileTruthTable asserts every mode/switch combination at the payload
// layer: answer and analysis presence and the answer-space reserve.
func TestPrecompileTruthTable(t *testing.T) {
	exam := loadIR(t, testGoldenIR)
	for _, row := range truthTable {
		t.Run(row.mode+"/answer="+boolName(row.showAnswer)+"/analysis="+boolName(row.showAnalysis), func(t *testing.T) {
			payload, err := Precompile(exam, layoutFor(row))
			if err != nil {
				t.Fatal(err)
			}
			var walk func(qs []RenderQuestion)
			walk = func(qs []RenderQuestion) {
				for _, q := range qs {
					if row.showAnswer && examHasAnswer(exam, q.Index) {
						if q.Answer == nil {
							t.Errorf("question %d: show_answer=true but payload has no answer", q.Index)
						}
					} else if q.Answer != nil {
						t.Errorf("question %d: show_answer=false but payload has an answer", q.Index)
					}
					if row.showAnalysis && examHasAnalysis(exam, q.Index) {
						if len(q.Analysis) == 0 {
							t.Errorf("question %d: show_analysis=true but payload has no analysis", q.Index)
						}
					} else if len(q.Analysis) > 0 {
						t.Errorf("question %d: show_analysis=false but payload has analysis", q.Index)
					}
					wantReserve := 0.0
					if row.mode == ModeStudent && !row.showAnswer {
						wantReserve = q.AnswerSpaceMM
					}
					if q.ReserveSpaceMM != wantReserve {
						t.Errorf("question %d: reserve_space_mm = %v, want %v (answer_space_mm=%v)", q.Index, q.ReserveSpaceMM, wantReserve, q.AnswerSpaceMM)
					}
					walk(q.Children)
				}
			}
			walk(payload.Exam.Questions)
		})
	}
}

func boolName(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// examHasAnswer reports whether the pre-order question at index has an answer
// in the source IR (the payload index is assigned in the same pre-order).
func examHasAnswer(exam ir.ExamDocumentIR, index int) bool {
	found := false
	i := 0
	var walk func(qs []ir.QuestionItem)
	walk = func(qs []ir.QuestionItem) {
		for _, q := range qs {
			if i == index {
				found = q.Answer != nil
			}
			i++
			walk(q.Children)
		}
	}
	walk(exam.Questions)
	return found
}

func examHasAnalysis(exam ir.ExamDocumentIR, index int) bool {
	found := false
	i := 0
	var walk func(qs []ir.QuestionItem)
	walk = func(qs []ir.QuestionItem) {
		for _, q := range qs {
			if i == index {
				found = len(q.Analysis) > 0
			}
			i++
			walk(q.Children)
		}
	}
	walk(exam.Questions)
	return found
}

// TestPrecompileTokenizesEveryTextField walks the golden exam and asserts that
// every text field reached the payload as tokens, including children, options,
// answer values/detail, analysis and table cells.
func TestPrecompileTokenizesEveryTextField(t *testing.T) {
	exam := loadIR(t, testGoldenIR)
	layout := loadLayout(t, testGoldenLayout+"/teacher.json")
	payload, err := Precompile(exam, layout)
	if err != nil {
		t.Fatal(err)
	}

	// Q1 stem: inline math inside the paragraph.
	q1 := payload.Exam.Questions[0]
	if findToken(q1.Stem[0].Tokens, inline.TypeMath) == nil {
		t.Errorf("Q1 stem lost its inline math: %+v", q1.Stem[0].Tokens)
	}
	// Q1 options: labels tokenized.
	for i, opt := range q1.Options {
		if len(opt.Label.Tokens) == 0 {
			t.Errorf("Q1 option %d label not tokenized", i)
		}
	}
	// Q1 answer short value and analysis.
	if q1.Answer == nil || len(q1.Answer.ShortValues) != 1 || len(q1.Answer.ShortValues[0].Tokens) == 0 {
		t.Errorf("Q1 answer short_values not tokenized: %+v", q1.Answer)
	}
	if len(q1.Analysis) == 0 || findToken(q1.Analysis[0].Tokens, inline.TypeMath) == nil {
		t.Errorf("Q1 analysis not tokenized with math: %+v", q1.Analysis)
	}
	// Q2 children: stem math and math_block kept verbatim.
	q2 := payload.Exam.Questions[1]
	if len(q2.Children) != 2 {
		t.Fatalf("Q2 children = %d, want 2", len(q2.Children))
	}
	c1 := q2.Children[0]
	if findToken(c1.Stem[0].Tokens, inline.TypeMath) == nil {
		t.Errorf("child (1) stem lost inline math")
	}
	if len(c1.Stem) < 2 || c1.Stem[1].Kind != BlockMath || c1.Stem[1].LaTeX != "a_n=2n+1" {
		t.Errorf("child (1) math_block altered: %+v", c1.Stem)
	}
	if len(c1.Stem[1].Tokens) != 0 {
		t.Errorf("math_block must not be inline-tokenized: %+v", c1.Stem[1].Tokens)
	}
	if c1.Answer == nil || c1.Answer.ShortValues[0].Tokens[0].Text != "11" {
		t.Errorf("child (1) answer not tokenized: %+v", c1.Answer)
	}
	c2 := q2.Children[1]
	if c2.Answer == nil || len(c2.Answer.Detail) == 0 || c2.Answer.Detail[0].Kind != BlockMath || c2.Answer.Detail[0].LaTeX != "S_n=n(n+2)" {
		t.Errorf("child (2) answer detail math_block altered: %+v", c2.Answer)
	}
	// Q3 table cells: header math and body text tokenized, spans normalized.
	q3 := payload.Exam.Questions[2]
	tbl := q3.Stem[1].Table
	if tbl == nil || tbl.Columns != 3 || len(tbl.Rows) != 3 {
		t.Fatalf("Q3 table = %+v, want 3 columns x 3 rows", tbl)
	}
	if tbl.Rows[0][0].RowSpan != 2 || tbl.Rows[0][1].ColSpan != 2 {
		t.Errorf("Q3 header spans = %+v", tbl.Rows[0])
	}
	if findToken(tbl.Rows[1][0].Tokens, inline.TypeMath) == nil {
		t.Errorf("Q3 header cell $f(1)$ not tokenized: %+v", tbl.Rows[1][0])
	}
	if tbl.Rows[2][0].Tokens[0].Text != "样例" {
		t.Errorf("Q3 body cell not tokenized: %+v", tbl.Rows[2][0])
	}
	// Every payload question index is the pre-order position.
	i := 0
	eachQuestion(payload, func(q *RenderQuestion) {
		if q.Index != i {
			t.Errorf("question index = %d, want pre-order %d", q.Index, i)
		}
		i++
	})
}

// TestPrecompileBlankAndStyles asserts markup survives into payload tokens.
func TestPrecompileBlankAndStyles(t *testing.T) {
	exam := loadIR(t, "../../testdata/render/ir/pages.json")
	layout := loadLayout(t, "../../testdata/render/layouts/pages-nobinding.json")
	payload, err := Precompile(exam, layout)
	if err != nil {
		t.Fatal(err)
	}
	stem := payload.Exam.Questions[1].Stem[0]
	blanks := 0
	for _, tok := range stem.Tokens {
		if tok.Type == inline.TypeBlank {
			blanks++
			if tok.Index != blanks {
				t.Errorf("blank index = %d, want %d", tok.Index, blanks)
			}
		}
	}
	if blanks != 2 {
		t.Errorf("stem has %d blanks, want 2: %+v", blanks, stem.Tokens)
	}
}

// TestPrecompileFloatGroup asserts the side-by-side grouping and its fallbacks.
func TestPrecompileFloatGroup(t *testing.T) {
	exam := loadIR(t, "../../testdata/render/ir/pages.json")
	layout := loadLayout(t, "../../testdata/render/layouts/pages-nobinding.json")
	payload, err := Precompile(exam, layout)
	if err != nil {
		t.Fatal(err)
	}
	stem := payload.Exam.Questions[0].Stem
	// The image groups with every adjacent text block up to the next non-text
	// block; both fixture paragraphs are beside the image.
	if len(stem) != 1 || stem[0].Kind != BlockFloatGroup {
		t.Fatalf("stem = %+v, want one float_group", stemKinds(stem))
	}
	group := stem[0]
	if len(group.Blocks) != 2 || group.Blocks[0].Kind != BlockParagraph || group.Blocks[1].Kind != BlockParagraph {
		t.Errorf("float group blocks = %+v, want two paragraphs", stemKinds(group.Blocks))
	}
	if group.WidthMM > layout2MaxFloat(layout) || group.WidthMM <= 0 {
		t.Errorf("float width %v out of bounds", group.WidthMM)
	}
	if group.Src != "assets/ast_01jrender000000000000000001.png" {
		t.Errorf("float src = %q", group.Src)
	}
	if len(payload.Warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", payload.Warnings)
	}
	// The golden image is the last stem block: visible right-aligned fallback.
	golden := loadIR(t, testGoldenIR)
	gp, err := Precompile(golden, layout)
	if err != nil {
		t.Fatal(err)
	}
	img := gp.Exam.Questions[0].Stem[1]
	if img.Kind != BlockImage || img.Align != "right" {
		t.Errorf("golden float fallback = %+v, want right-aligned image", img)
	}
	if !hasWarning(gp.Warnings, "float_fallback", "questions[0].stem[1]") {
		t.Errorf("missing float_fallback warning: %+v", gp.Warnings)
	}
}

func stemKinds(blocks []RenderBlock) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Kind)
	}
	return out
}

func layout2MaxFloat(layout ir.LayoutSpec) float64 {
	lay, _ := newRenderLayout(layout)
	return lay.MaxFloatWidthMM
}

func hasWarning(warnings []RenderWarning, code, path string) bool {
	for _, w := range warnings {
		if w.Code == code && w.Path == path {
			return true
		}
	}
	return false
}

// TestPrecompileUnknownBlock asserts unknown blocks warn and keep their source.
func TestPrecompileUnknownBlock(t *testing.T) {
	exam := ir.ExamDocumentIR{
		SchemaVersion: "1.0.0", ExamID: "exam_unknown",
		Metadata: ir.ExamMetadata{Title: "未知块", Subject: "other"},
		Assets:   map[string]ir.Asset{},
		Questions: []ir.QuestionItem{{
			Kind: "single", DisplayNumber: "1",
			Stem: []ir.ContentBlock{{Type: "chem_block", LaTeX: "H2O"}},
		}},
	}
	payload, err := Precompile(exam, layoutFor(truthRow{ModeStudent, false, false}))
	if err != nil {
		t.Fatal(err)
	}
	blk := payload.Exam.Questions[0].Stem[0]
	if blk.Kind != BlockUnknown || blk.Source != "chem_block" {
		t.Errorf("unknown block = %+v", blk)
	}
	if !strings.Contains(string(blk.Raw), "H2O") {
		t.Errorf("unknown block raw lost fields: %s", blk.Raw)
	}
	if !hasWarning(payload.Warnings, "unknown_block", "questions[0].stem[0]") {
		t.Errorf("missing unknown_block warning: %+v", payload.Warnings)
	}
}

// TestPrecompileMissingAsset asserts an undeclared asset reference is an error.
func TestPrecompileMissingAsset(t *testing.T) {
	exam := ir.ExamDocumentIR{
		SchemaVersion: "1.0.0", ExamID: "exam_missing_asset",
		Metadata: ir.ExamMetadata{Title: "缺资产", Subject: "other"},
		Assets:   map[string]ir.Asset{},
		Questions: []ir.QuestionItem{{
			Kind: "single", DisplayNumber: "1",
			Stem: []ir.ContentBlock{{Type: "image_ref", AssetID: "ast_01jmissing00000000000000001", LayoutHint: "inline"}},
		}},
	}
	_, err := Precompile(exam, layoutFor(truthRow{ModeStudent, false, false}))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

// TestPrecompileInvalidInputs covers the renderer's own runtime guards.
func TestPrecompileInvalidInputs(t *testing.T) {
	exam := loadIR(t, testGoldenIR)
	base := layoutFor(truthRow{ModeStudent, false, false})
	cases := []struct {
		name   string
		mutate func(*ir.LayoutSpec)
	}{
		{"paper", func(l *ir.LayoutSpec) { l.PaperSize = "letter" }},
		{"columns", func(l *ir.LayoutSpec) { l.Columns = 3 }},
		{"mode", func(l *ir.LayoutSpec) { l.Mode = "reviewer" }},
		{"option_layout", func(l *ir.LayoutSpec) { l.OptionLayout = "3x3" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layout := base
			tc.mutate(&layout)
			if _, err := Precompile(exam, layout); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
	t.Run("answer_space", func(t *testing.T) {
		bad := exam
		bad.Questions = append([]ir.QuestionItem(nil), exam.Questions...)
		bad.Questions[0].AnswerSpaceMM = math.NaN()
		if _, err := Precompile(bad, base); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("err = %v, want ErrInvalidInput", err)
		}
	})
}

// TestPrecompileAssetDedup asserts one asset referenced many times is listed
// once with a shared workspace path, and different IDs stay separate.
func TestPrecompileAssetDedup(t *testing.T) {
	exam := loadIR(t, "../../testdata/render/ir/options.json")
	payload, err := Precompile(exam, loadLayout(t, "../../testdata/render/layouts/measure-a4-1col.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Assets) != 1 {
		t.Fatalf("assets = %d, want 1 (four references to one ID): %+v", len(payload.Assets), payload.Assets)
	}
	a := payload.Assets[0]
	if a.AssetID != "ast_01jrender000000000000000001" || a.Path != "assets/ast_01jrender000000000000000001.png" {
		t.Errorf("asset = %+v", a)
	}
	refs := 0
	eachQuestion(payload, func(q *RenderQuestion) {
		for _, opt := range q.Options {
			for _, b := range opt.Content {
				if b.Kind == BlockImage {
					refs++
					if b.Src != a.Path {
						t.Errorf("image src %q != shared asset path %q", b.Src, a.Path)
					}
				}
			}
		}
	})
	if refs != 4 {
		t.Errorf("image references = %d, want 4", refs)
	}
}

// TestPrecompileLayoutConstants pins the precomputed page geometry.
func TestPrecompileLayoutConstants(t *testing.T) {
	exam := loadIR(t, testGoldenIR)
	cases := []struct {
		paper   string
		columns int
		binding bool
		width   float64
		height  float64
		typst   string
		left    float64
	}{
		{"a4", 1, false, 210, 297, "a4", MarginLeftMM},
		{"a4", 1, true, 210, 297, "a4", MarginLeftMM + BindingReserveMM},
		{"a3", 2, true, 297, 420, "a3", MarginLeftMM + BindingReserveMM},
		{"b4", 2, false, 257, 364, "jis-b4", MarginLeftMM},
	}
	for _, tc := range cases {
		layout := layoutFor(truthRow{ModeStudent, false, false})
		layout.PaperSize, layout.Columns, layout.ShowBindingLine = tc.paper, tc.columns, tc.binding
		payload, err := Precompile(exam, layout)
		if err != nil {
			t.Fatal(err)
		}
		lay := payload.Layout
		if lay.PageWidthMM != tc.width || lay.PageHeightMM != tc.height || lay.TypstPaper != tc.typst {
			t.Errorf("%s: page = %v x %v (%s)", tc.paper, lay.PageWidthMM, lay.PageHeightMM, lay.TypstPaper)
		}
		if lay.Margin.LeftMM != tc.left {
			t.Errorf("%s binding=%v: left margin = %v, want %v", tc.paper, tc.binding, lay.Margin.LeftMM, tc.left)
		}
		wantText := tc.width - tc.left - MarginRightMM
		if lay.TextWidthMM != wantText {
			t.Errorf("%s: text width = %v, want %v", tc.paper, lay.TextWidthMM, wantText)
		}
		wantCol := (wantText - float64(tc.columns-1)*ColumnGutterMM) / float64(tc.columns)
		if lay.ColumnWidthMM != wantCol {
			t.Errorf("%s %dcol: column width = %v, want %v", tc.paper, tc.columns, lay.ColumnWidthMM, wantCol)
		}
	}
}

// TestPrecompileDeterministic asserts identical inputs produce identical
// payloads and that the payload carries no host paths.
func TestPrecompileDeterministic(t *testing.T) {
	exam := loadIR(t, testGoldenIR)
	layout := loadLayout(t, testGoldenLayout+"/student.json")
	a, err := Precompile(exam, layout)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Precompile(exam, layout)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Error("Precompile is not deterministic for identical inputs")
	}
	if strings.Contains(string(ja), "/home/") || strings.Contains(string(ja), "/tmp/") {
		t.Error("payload leaks a host path")
	}
}

// TestChooseOptionGrid pins the uniform ladder, including unmeasurable input.
func TestChooseOptionGrid(t *testing.T) {
	const gutter = 6.0
	cases := []struct {
		name        string
		requested   string
		wmax, avail float64
		want        string
	}{
		{"explicit kept", Grid2x2, 1000, 10, Grid2x2},
		{"four fit", OptionAuto, 10, 100, Grid1x4},
		{"four fit exactly", OptionAuto, 20.5, 100, Grid1x4}, // 4*20.5+18 = 100
		{"two fit", OptionAuto, 30, 100, Grid2x2},
		{"two fit exactly", OptionAuto, 47, 100, Grid2x2}, // 2*47+6 = 100
		{"one per row", OptionAuto, 60, 100, Grid4x1},
		{"nan", OptionAuto, math.NaN(), 100, Grid4x1},
		{"inf", OptionAuto, math.Inf(1), 100, Grid4x1},
		{"negative width", OptionAuto, -1, 100, Grid4x1},
		{"nan avail", OptionAuto, 10, math.NaN(), Grid4x1},
		{"negative avail", OptionAuto, 10, -5, Grid4x1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseOptionGrid(tc.requested, tc.wmax, tc.avail, gutter); got != tc.want {
				t.Errorf("chooseOptionGrid(%q, %v, %v, %v) = %q, want %q", tc.requested, tc.wmax, tc.avail, gutter, got, tc.want)
			}
		})
	}
}

// TestBuildTableGrid covers span placement, hole padding and overlap errors.
func TestBuildTableGrid(t *testing.T) {
	t.Run("golden spans", func(t *testing.T) {
		exam := loadIR(t, testGoldenIR)
		grid, err := buildTableGrid(*exam.Questions[2].Stem[1].Table)
		if err != nil {
			t.Fatal(err)
		}
		if grid.Columns != 3 || len(grid.Rows) != 3 {
			t.Fatalf("grid = %d cols x %d rows", grid.Columns, len(grid.Rows))
		}
		// Row 1 is covered by the rowspan of 项目: only the two math cells.
		if len(grid.Rows[1]) != 2 {
			t.Errorf("row 1 origins = %d, want 2", len(grid.Rows[1]))
		}
	})
	t.Run("ragged hole padded", func(t *testing.T) {
		grid, err := buildTableGrid(ir.Table{
			Headers: [][]ir.TableCell{{{Text: "a", ColSpan: 2}, {Text: "b"}}},
			Rows:    [][]ir.TableCell{{{Text: "c"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if grid.Columns != 3 || len(grid.Rows[1]) != 3 {
			t.Fatalf("grid = %+v", grid)
		}
		if grid.Rows[1][1].Text != "" || grid.Rows[1][2].Text != "" {
			t.Errorf("holes not padded with empty cells: %+v", grid.Rows[1])
		}
	})
	t.Run("overlap rejected", func(t *testing.T) {
		// Row 0: a (col 0), b (rowspan 2, col 1).
		// Row 1: c (colspan 2) starts at the first free slot, col 0, and its
		// span reaches col 1, which b's rowspan occupies: impossible grid.
		_, err := buildTableGrid(ir.Table{
			Headers: [][]ir.TableCell{{{Text: "a"}, {Text: "b", RowSpan: 2}}},
			Rows:    [][]ir.TableCell{{{Text: "c", ColSpan: 2}}},
		})
		if err == nil {
			t.Fatal("overlapping spans accepted")
		}
	})
	t.Run("rowspan beyond table rejected", func(t *testing.T) {
		_, err := buildTableGrid(ir.Table{
			Rows: [][]ir.TableCell{{{Text: "a", RowSpan: 2}}},
		})
		if err == nil {
			t.Fatal("rowspan beyond the table accepted")
		}
	})
	t.Run("empty table rejected", func(t *testing.T) {
		if _, err := buildTableGrid(ir.Table{}); err == nil {
			t.Fatal("empty table accepted")
		}
	})
}
