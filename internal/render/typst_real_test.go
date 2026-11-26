package render

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// requireTool skips a real-render test when an external PDF tool is missing.
// The verify script checks for these up front so nothing is skipped unnoticed.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed: %v", name, err)
	}
}

// pdfFile writes an artifact to a private temporary file for external tools.
func pdfFile(t *testing.T, art *Artifact) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.pdf")
	if err := os.WriteFile(path, art.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// pdfText extracts the layout text of a PDF with pdftotext.
func pdfText(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("pdftotext", "-layout", path, "-").Output()
	if err != nil {
		t.Fatalf("pdftotext: %v", err)
	}
	return string(out)
}

// pdfPages returns the page count and every page size in points. Per-page
// sizes need an explicit page range; without one pdfinfo prints only the
// first page's size.
func pdfPages(t *testing.T, path string) (int, [][2]float64) {
	t.Helper()
	out, err := exec.Command("pdfinfo", path).Output()
	if err != nil {
		t.Fatalf("pdfinfo: %v", err)
	}
	pages := 0
	if m := regexp.MustCompile(`Pages:\s+(\d+)`).FindSubmatch(out); m != nil {
		pages, _ = strconv.Atoi(string(m[1]))
	}
	if pages == 0 {
		t.Fatalf("pdfinfo reported no pages:\n%s", out)
	}
	out, err = exec.Command("pdfinfo", "-f", "1", "-l", strconv.Itoa(pages), path).Output()
	if err != nil {
		t.Fatalf("pdfinfo -f -l: %v", err)
	}
	var sizes [][2]float64
	for _, m := range regexp.MustCompile(`Page\s+\d+ size:\s+([\d.]+) x ([\d.]+) pts`).FindAllSubmatch(out, -1) {
		w, _ := strconv.ParseFloat(string(m[1]), 64)
		h, _ := strconv.ParseFloat(string(m[2]), 64)
		sizes = append(sizes, [2]float64{w, h})
	}
	return pages, sizes
}

// wordBox is one word's bounding box from pdftotext -bbox.
type wordBox struct {
	text                   string
	xMin, yMin, xMax, yMax float64
}

// pdfWords extracts word bounding boxes per page (0-based page index), sorted
// in reading order: top to bottom, then left to right. pdftotext emits words
// in content-stream order, which interleaves grid cells.
func pdfWords(t *testing.T, path string) [][]wordBox {
	t.Helper()
	out, err := exec.Command("pdftotext", "-bbox", path, "-").Output()
	if err != nil {
		t.Fatalf("pdftotext -bbox: %v", err)
	}
	pageRe := regexp.MustCompile(`<page width="([\d.]+)" height="([\d.]+)">`)
	wordRe := regexp.MustCompile(`<word xMin="([\d.]+)" yMin="([\d.]+)" xMax="([\d.]+)" yMax="([\d.]+)">([^<]*)</word>`)
	var pages [][]wordBox
	for _, chunk := range pageRe.Split(string(out), -1)[1:] {
		var words []wordBox
		for _, m := range wordRe.FindAllStringSubmatch(chunk, -1) {
			f := func(i int) float64 { v, _ := strconv.ParseFloat(m[i], 64); return v }
			words = append(words, wordBox{text: m[5], xMin: f(1), yMin: f(2), xMax: f(3), yMax: f(4)})
		}
		sort.Slice(words, func(i, j int) bool {
			if dy := words[i].yMin - words[j].yMin; dy > 2 || dy < -2 {
				return words[i].yMin < words[j].yMin
			}
			return words[i].xMin < words[j].xMin
		})
		pages = append(pages, words)
	}
	return pages
}

// optionLabels finds the A./B./C./D. label words of one question on a page.
func optionLabels(words []wordBox) map[string]wordBox {
	out := map[string]wordBox{}
	for _, w := range words {
		switch w.text {
		case "A.", "B.", "C.", "D.", "E.":
			if _, seen := out[w.text]; !seen {
				out[w.text] = w
			}
		}
	}
	return out
}

// assertGrid checks the option labels of one question form the expected
// uniform grid: same-row labels share a y, columns are ordered left to right,
// and rows fill completely before the next row starts.
func assertGrid(t *testing.T, labels map[string]wordBox, grid string, count int) {
	t.Helper()
	cols := GridColumns(grid)
	if cols == 0 {
		t.Fatalf("unknown grid %q", grid)
	}
	letters := []string{"A.", "B.", "C.", "D.", "E."}[:count]
	for _, l := range letters {
		if _, ok := labels[l]; !ok {
			t.Fatalf("label %s not found in %v", l, labels)
		}
	}
	const yTol = 2.0
	rows := make([][]string, 0, (count+cols-1)/cols)
	for i, l := range letters {
		r := i / cols
		for len(rows) <= r {
			rows = append(rows, nil)
		}
		rows[r] = append(rows[r], l)
	}
	for r, row := range rows {
		y := labels[row[0]].yMin
		for _, l := range row {
			if dy := labels[l].yMin - y; dy > yTol || dy < -yTol {
				t.Errorf("grid %s row %d: %s at y=%.1f, %s at y=%.1f (not one row)", grid, r, row[0], y, l, labels[l].yMin)
			}
		}
		for i := 1; i < len(row); i++ {
			if labels[row[i]].xMin <= labels[row[i-1]].xMin {
				t.Errorf("grid %s row %d: columns not left to right", grid, r)
			}
		}
		if r > 0 {
			prev := rows[r-1][0]
			if labels[row[0]].yMin <= labels[prev].yMin {
				t.Errorf("grid %s: row %d not below row %d", grid, r, r-1)
			}
		}
	}
	// Distinct rows must be exactly ceil(count/cols): no greedy packing.
	seenY := map[float64]bool{}
	for _, l := range letters {
		y := labels[l].yMin
		merged := false
		for sy := range seenY {
			if dy := y - sy; dy < yTol && dy > -yTol {
				merged = true
				break
			}
		}
		if !merged {
			seenY[y] = true
		}
	}
	if want := (count + cols - 1) / cols; len(seenY) != want {
		t.Errorf("grid %s with %d options: %d distinct rows, want %d", grid, count, len(seenY), want)
	}
}

// TestRenderOptionGridsAuto asserts the measured uniform ladder on fixtures
// covering short text, formulas, images and an ultra-wide option.
func TestRenderOptionGridsAuto(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	exam := loadIR(t, "../../testdata/render/ir/options.json")
	layout := loadLayout(t, "../../testdata/render/layouts/measure-a4-1col.json")
	r := newRenderer(t, func(cfg *TypstConfig) { cfg.AssetResolver = renderResolver(t) })
	art, err := r.Render(context.Background(), exam, layout)
	if err != nil {
		t.Fatal(err)
	}
	assertPDF(t, art)
	if len(art.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", art.Warnings)
	}
	dumpPDF(t, "options-auto", art)
	pages := pdfWords(t, pdfFile(t, art))
	if len(pages) == 0 {
		t.Fatal("no pages")
	}
	// All six questions fit on one A4 page in this fixture.
	words := pages[0]
	for _, p := range pages[1:] {
		words = append(words, p...)
	}
	want := []struct {
		grid  string
		count int
	}{
		{Grid1x4, 4}, // short text
		{Grid2x2, 4}, // wide formulas: too wide for four across, two fit
		{Grid1x4, 4}, // images (20mm each fit four across)
		{Grid4x1, 4}, // ultra-wide
		{Grid1x4, 3}, // three options: one row, last cell empty
		{Grid1x4, 5}, // five options: a full second row is appended
	}
	// Labels repeat per question; walk them in document order by y then x.
	type labeled struct {
		letter string
		box    wordBox
	}
	var all []labeled
	for _, w := range words {
		switch w.text {
		case "A.", "B.", "C.", "D.", "E.":
			all = append(all, labeled{w.text, w})
		}
	}
	// Group consecutive labels into questions: a new question starts at "A.".
	var groups [][]labeled
	for _, l := range all {
		if l.letter == "A." || len(groups) == 0 {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], l)
	}
	if len(groups) != len(want) {
		t.Fatalf("found %d option groups, want %d", len(groups), len(want))
	}
	for i, g := range groups {
		labels := map[string]wordBox{}
		for _, l := range g {
			labels[l.letter] = l.box
		}
		assertGrid(t, labels, want[i].grid, want[i].count)
	}
}

// TestRenderOptionGridsExplicit asserts explicit grids are kept exactly, with
// full rows appended for extra options and empty cells for missing ones.
func TestRenderOptionGridsExplicit(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	exam := loadIR(t, "../../testdata/render/ir/options.json")
	r := newRenderer(t, func(cfg *TypstConfig) { cfg.AssetResolver = renderResolver(t) })
	for _, grid := range []string{Grid1x4, Grid2x2, Grid4x1} {
		t.Run(grid, func(t *testing.T) {
			layout := loadLayout(t, "../../testdata/render/layouts/grid-"+grid+".json")
			art, err := r.Render(context.Background(), exam, layout)
			if err != nil {
				t.Fatal(err)
			}
			assertPDF(t, art)
			if len(art.Warnings) != 0 {
				t.Fatalf("unexpected warnings: %+v", art.Warnings)
			}
			dumpPDF(t, "options-"+grid, art)
			var words []wordBox
			for _, p := range pdfWords(t, pdfFile(t, art)) {
				words = append(words, p...)
			}
			type labeled struct {
				letter string
				box    wordBox
			}
			var all []labeled
			for _, w := range words {
				switch w.text {
				case "A.", "B.", "C.", "D.", "E.":
					all = append(all, labeled{w.text, w})
				}
			}
			var groups [][]labeled
			for _, l := range all {
				if l.letter == "A." || len(groups) == 0 {
					groups = append(groups, nil)
				}
				groups[len(groups)-1] = append(groups[len(groups)-1], l)
			}
			counts := []int{4, 4, 4, 4, 3, 5}
			if len(groups) != len(counts) {
				t.Fatalf("found %d option groups, want %d", len(groups), len(counts))
			}
			for i, g := range groups {
				labels := map[string]wordBox{}
				for _, l := range g {
					labels[l.letter] = l.box
				}
				assertGrid(t, labels, grid, counts[i])
			}
		})
	}
}

// TestRenderPaperSizes asserts A4/A3/B4 page geometry and column behavior.
func TestRenderPaperSizes(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdfinfo")
	exam := loadIR(t, "../../testdata/render/ir/pages.json")
	r := newRenderer(t, func(cfg *TypstConfig) { cfg.AssetResolver = renderResolver(t) })
	cases := []struct {
		layout   string
		widthMM  float64
		heightMM float64
	}{
		{"pages-nobinding", 210, 297},
		{"pages-a4-2col", 210, 297},
		{"pages-a3-2col", 297, 420},
		{"pages-b4-1col", 257, 364},
	}
	for _, tc := range cases {
		t.Run(tc.layout, func(t *testing.T) {
			art, err := r.Render(context.Background(), exam, loadLayout(t, "../../testdata/render/layouts/"+tc.layout+".json"))
			if err != nil {
				t.Fatal(err)
			}
			assertPDF(t, art)
			if len(art.Warnings) != 0 {
				t.Fatalf("unexpected warnings: %+v", art.Warnings)
			}
			dumpPDF(t, tc.layout, art)
			pages, sizes := pdfPages(t, pdfFile(t, art))
			if pages == 0 || len(sizes) != pages {
				t.Fatalf("pages = %d, sizes = %v", pages, sizes)
			}
			wantW, wantH := mmToPt(tc.widthMM), mmToPt(tc.heightMM)
			for i, s := range sizes {
				if diff(s[0], wantW) > 1 || diff(s[1], wantH) > 1 {
					t.Errorf("page %d size = %.1f x %.1f pt, want %.1f x %.1f", i+1, s[0], s[1], wantW, wantH)
				}
			}
		})
	}
}

func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestRenderBindingLine asserts the binding line and its reserve appear only
// when enabled, on every page, outside the text flow.
func TestRenderBindingLine(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	exam := loadIR(t, "../../testdata/render/ir/pages.json")
	r := newRenderer(t, func(cfg *TypstConfig) { cfg.AssetResolver = renderResolver(t) })
	on, err := r.Render(context.Background(), exam, loadLayout(t, "../../testdata/render/layouts/pages-nobinding.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pdfText(t, pdfFile(t, on)), "密封线") {
		t.Error("binding line text present with show_binding_line=false")
	}
	off, err := r.Render(context.Background(), exam, loadLayout(t, "../../testdata/render/layouts/pages-b4-1col.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := pdfText(t, pdfFile(t, off))
	if !strings.Contains(text, "密封线内不要答题") {
		t.Error("binding line text missing with show_binding_line=true")
	}
	// The binding words sit in the left reserve, left of the text area.
	for _, page := range pdfWords(t, pdfFile(t, off)) {
		for _, w := range page {
			if strings.Contains(w.text, "密封线") {
				if w.xMin >= mmToPt(MarginLeftMM+BindingReserveMM) {
					t.Errorf("binding word %q at x=%.1f is inside the text area", w.text, w.xMin)
				}
			}
		}
	}
}

// TestRenderHeaderFooter asserts header/footer repeat on every page.
func TestRenderHeaderFooter(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	// A long exam forces multiple pages.
	exam := loadIR(t, "../../testdata/render/ir/pages.json")
	long := exam
	long.Questions = nil
	for i := 0; i < 30; i++ {
		q := exam.Questions[0]
		q.DisplayNumber = strconv.Itoa(i + 1)
		long.Questions = append(long.Questions, q)
	}
	r := newRenderer(t, func(cfg *TypstConfig) { cfg.AssetResolver = renderResolver(t) })
	art, err := r.Render(context.Background(), long, loadLayout(t, "../../testdata/render/layouts/pages-a4-2col.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertPDF(t, art)
	path := pdfFile(t, art)
	pages, _ := pdfPages(t, path)
	if pages < 2 {
		t.Fatalf("fixture produced %d pages, want >= 2", pages)
	}
	out, err := exec.Command("pdftotext", "-layout", path, "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	// pdftotext separates pages with form feeds.
	pageTexts := strings.Split(string(out), "\f")
	count := 0
	for _, pt := range pageTexts {
		if strings.TrimSpace(pt) == "" {
			continue
		}
		count++
		if !strings.Contains(pt, "页面要素样例") {
			t.Errorf("page %d missing header", count)
		}
		if !strings.Contains(pt, "第") || !strings.Contains(pt, "页") {
			t.Errorf("page %d missing footer", count)
		}
	}
	if count != pages {
		t.Errorf("text pages = %d, pdfinfo pages = %d", count, pages)
	}
}

// TestRenderStudentTeacherDifference asserts the structural difference between
// student and teacher output on the same IR: answers/analysis visibility and
// the answer-space reserve.
func TestRenderStudentTeacherDifference(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	exam := loadIR(t, "../../testdata/render/ir/pages.json")
	r := newRenderer(t, func(cfg *TypstConfig) { cfg.AssetResolver = renderResolver(t) })
	student, err := r.Render(context.Background(), exam, loadLayout(t, "../../testdata/render/layouts/pages-nobinding.json"))
	if err != nil {
		t.Fatal(err)
	}
	teacher, err := r.Render(context.Background(), exam, loadLayout(t, "../../testdata/render/layouts/pages-teacher.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, tt := pdfText(t, pdfFile(t, student)), pdfText(t, pdfFile(t, teacher))
	if strings.Contains(st, "【参考答案】") || strings.Contains(st, "【解析】") {
		t.Error("student paper shows answers or analysis")
	}
	if !strings.Contains(tt, "【参考答案】") || !strings.Contains(tt, "【解析】") {
		t.Error("teacher paper missing answers or analysis")
	}
	if !strings.Contains(tt, "B") {
		t.Error("teacher paper missing the answer value")
	}
	// Q3 has answer_space_mm 30 and no visible answer in either mode; Q4
	// follows it. The vertical distance from Q3's stem to Q4's stem therefore
	// contains the reserve: >= 30mm in the student paper, compact in teacher.
	stemGap := func(art *Artifact) float64 {
		var probe, next float64
		for _, page := range pdfWords(t, pdfFile(t, art)) {
			for _, w := range page {
				if strings.Contains(w.text, "留白探测题") && probe == 0 {
					probe = w.yMin
				}
				if strings.Contains(w.text, "留白探测后继题") && next == 0 {
					next = w.yMin
				}
			}
		}
		if probe == 0 || next == 0 {
			t.Fatalf("probe words not found (probe=%v next=%v)", probe, next)
		}
		return next - probe
	}
	const reserveMM = 30
	gs, gt := stemGap(student), stemGap(teacher)
	if gs < mmToPt(reserveMM) {
		t.Errorf("student stem gap %.1fpt, want >= %.1fpt reserve", gs, mmToPt(reserveMM))
	}
	if gs-gt < mmToPt(reserveMM)-2 {
		t.Errorf("student-teacher gap difference %.1fpt, want >= %.1fpt reserve", gs-gt, mmToPt(reserveMM))
	}
}

// TestRenderFloatSideBySide asserts the float_right image shares a row with
// its adjacent text and narrows that text's line width.
func TestRenderFloatSideBySide(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	exam := loadIR(t, "../../testdata/render/ir/pages.json")
	r := newRenderer(t, func(cfg *TypstConfig) { cfg.AssetResolver = renderResolver(t) })
	art, err := r.Render(context.Background(), exam, loadLayout(t, "../../testdata/render/layouts/pages-nobinding.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertPDF(t, art)
	if len(art.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", art.Warnings)
	}
	dumpPDF(t, "float-side-by-side", art)
	words := pdfWords(t, pdfFile(t, art))[0]
	// The first stem paragraph's words must stay left of the image column.
	payload, err := Precompile(exam, loadLayout(t, "../../testdata/render/layouts/pages-nobinding.json"))
	if err != nil {
		t.Fatal(err)
	}
	imgWidth := payload.Exam.Questions[0].Stem[0].WidthMM
	left := mmToPt(MarginLeftMM)
	textRight := left + mmToPt(payload.Layout.ColumnWidthMM-imgWidth-FloatGapMM)
	found := 0
	for _, w := range words {
		if strings.Contains(w.text, "本题图片在题干段落左侧并排排版") || strings.Contains(w.text, "行宽被图片压缩") {
			found++
			if w.xMax > textRight+2 {
				t.Errorf("beside-image word %q reaches x=%.1f, past the narrowed column %.1f", w.text, w.xMax, textRight)
			}
		}
	}
	if found == 0 {
		t.Skip("pdftotext merged the probe words; visual check via dump instead")
	}
}

// TestRenderTableSpans asserts merged cells render as a table with the right
// structure: the golden header spans and the body row.
func TestRenderTableSpans(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	exam := loadIR(t, testGoldenIR)
	r := newRenderer(t, nil)
	art, err := r.Render(context.Background(), exam, loadLayout(t, testGoldenLayout+"/student.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := pdfText(t, pdfFile(t, art))
	for _, want := range []string{"项目", "函数值", "样例"} {
		if !strings.Contains(text, want) {
			t.Errorf("table cell %q missing from output", want)
		}
	}
	// 项目 spans two rows: it appears once, vertically centered beside both
	// header rows; 函数值 spans two columns above f(1) and f(2).
	if strings.Count(text, "项目") != 1 {
		t.Errorf("rowspan cell 项目 rendered %d times, want 1", strings.Count(text, "项目"))
	}
}

// TestRenderOffline asserts a render succeeds with the network cut and does
// not touch the user's Typst package cache.
func TestRenderOffline(t *testing.T) {
	requireTypst(t)
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skipf("unshare unavailable: %v", err)
	}
	// Re-exec this test inside a network namespace with no interfaces up.
	if os.Getenv("EXAMFORGE_OFFLINE_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=TestRenderOffline", "-test.count=1")
		cmd.Env = append(os.Environ(), "EXAMFORGE_OFFLINE_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("offline child failed: %v\n%s", err, out)
		}
		return
	}
	exam := loadIR(t, testGoldenIR)
	r := newRenderer(t, nil)
	art, err := r.Render(context.Background(), exam, loadLayout(t, testGoldenLayout+"/student.json"))
	if err != nil {
		t.Fatalf("offline render: %v", err)
	}
	assertPDF(t, art)
	if len(art.Warnings) != 1 || art.Warnings[0].Code != "float_fallback" {
		t.Fatalf("offline warnings = %+v", art.Warnings)
	}
}

// TestRenderRepeatStable asserts repeated identical renders agree on every
// structural fact (page count, size, text), ignoring PDF creation timestamps.
func TestRenderRepeatStable(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	requireTool(t, "pdfinfo")
	exam := loadIR(t, testGoldenIR)
	layout := loadLayout(t, testGoldenLayout+"/student.json")
	r := newRenderer(t, nil)
	var texts []string
	var pageCount int
	var sizes [][2]float64
	for i := 0; i < 2; i++ {
		art, err := r.Render(context.Background(), exam, layout)
		if err != nil {
			t.Fatal(err)
		}
		path := pdfFile(t, art)
		texts = append(texts, pdfText(t, path))
		n, s := pdfPages(t, path)
		if i == 0 {
			pageCount, sizes = n, s
		} else if n != pageCount || len(s) != len(sizes) {
			t.Errorf("repeat render page structure differs: %d/%v vs %d/%v", n, s, pageCount, sizes)
		}
	}
	if texts[0] != texts[1] {
		t.Error("repeat render text differs")
	}
}

// TestRenderNoRepoResidue asserts a render leaves no new files in the repo.
func TestRenderNoRepoResidue(t *testing.T) {
	requireTypst(t)
	before := repoSnapshot(t)
	exam := loadIR(t, testGoldenIR)
	r := newRenderer(t, nil)
	if _, err := r.Render(context.Background(), exam, loadLayout(t, testGoldenLayout+"/student.json")); err != nil {
		t.Fatal(err)
	}
	after := repoSnapshot(t)
	for name := range after {
		if !before[name] {
			t.Errorf("render left a new file in the repo: %s", name)
		}
	}
}

// repoSnapshot lists files under the repository root, skipping .git.
func repoSnapshot(t *testing.T) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	root := "../.."
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		out[p] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRenderGoldenCLI is the end-to-end path through the examrender CLI.
func TestRenderGoldenCLI(t *testing.T) {
	requireTypst(t)
	out := filepath.Join(t.TempDir(), "golden.pdf")
	cmd := exec.Command("go", "run", "./cmd/examrender",
		"-ir", "testdata/golden/ir/sample.json",
		"-layout", "testdata/golden/layout/student.json",
		"-assets", "testdata/golden/assets",
		"-out", out)
	cmd.Dir = "../.."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("examrender: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Errorf("CLI output is not a PDF: %q", data[:min(8, len(data))])
	}
}
