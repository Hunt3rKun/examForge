package cloud

import (
	"fmt"
	"sort"
	"strings"

	"examforge/internal/ir"
	"examforge/internal/parser"
)

// candidate is the merged, pre-remap document: page extractions folded into
// IR sections with model-local asset keys still in place, plus the extraction
// metadata the deterministic merge and the crop stage need.
type candidate struct {
	exam      ir.ExamDocumentIR
	assets    map[string]assetOut // model key -> declared asset
	questions []questionSlot      // flat reading-order list of merged questions
	warnings  []string
}

type questionSlot struct {
	page     int
	index    int // index within the page's flattened question list
	ref      string
	section  int         // index into exam.Sections
	position int         // index into that section's Questions at build time
	out      questionOut // extraction metadata (continues, answer_for, bbox)
	removed  bool
}

// item resolves the slot's current question value. Sections are rebuilt after
// removals, so slots address questions by (section, position), never by
// pointers that a rebuild would invalidate.
func (c *candidate) item(s *questionSlot) *ir.QuestionItem {
	return &c.exam.Sections[s.section].Questions[s.position]
}

// mergePages folds page extractions, already sorted by page number, into one
// candidate document. Deterministic rules run first; only genuinely ambiguous
// cross-page relations are left for the optional text merge.
func mergePages(pages []pageExtraction) (*candidate, error) {
	c := &candidate{
		assets: map[string]assetOut{},
		exam: ir.ExamDocumentIR{
			SchemaVersion: "1.0.0",
			Metadata:      ir.ExamMetadata{Title: "解析试卷", Subject: "other"},
			Assets:        map[string]ir.Asset{},
		},
	}
	sectionByTitle := map[string]int{}
	for _, page := range pages {
		for _, asset := range page.Assets {
			if asset.Key == "" {
				return nil, parser.Permanent("ir_asset_invalid", fmt.Errorf("page %d declares an asset with an empty key", page.Page))
			}
			if _, dup := c.assets[asset.Key]; dup {
				return nil, parser.Permanent("ir_asset_invalid", fmt.Errorf("asset key %q is declared on more than one page", asset.Key))
			}
			c.assets[asset.Key] = asset
		}
		for _, sec := range page.Sections {
			title := strings.TrimSpace(sec.Title)
			if title == "" {
				title = "试题"
			}
			si, ok := sectionByTitle[title]
			if !ok {
				si = len(c.exam.Sections)
				sectionByTitle[title] = si
				c.exam.Sections = append(c.exam.Sections, ir.Section{
					SectionID:   fmt.Sprintf("sec_%d", si+1),
					Title:       title,
					Instruction: sec.Instruction,
				})
			}
			for qi := range sec.Questions {
				out := sec.Questions[qi]
				q, err := toIRQuestion(out)
				if err != nil {
					return nil, parser.Permanent("ir_question_invalid", fmt.Errorf("page %d question %q: %w", page.Page, out.DisplayNumber, err))
				}
				// Provenance keeps the page through the bbox page number; the
				// IR schema's provenance object has no free-form page field.
				if out.BBox != nil {
					if bb, err := toIRBBox(out.BBox); err == nil {
						q.Provenance = map[string]any{"bboxes": []ir.BBox{*bb}}
					}
				}
				if q.Provenance == nil {
					q.Provenance = map[string]any{"bboxes": []ir.BBox{{Page: page.Page, Rect: [4]float64{0, 0, 1, 1}}}}
				}
				c.exam.Sections[si].Questions = append(c.exam.Sections[si].Questions, *q)
				slot := questionSlot{
					page:     page.Page,
					index:    qi,
					ref:      fmt.Sprintf("p%d.q%d", page.Page, qi),
					section:  si,
					position: len(c.exam.Sections[si].Questions) - 1,
					out:      out,
				}
				c.questions = append(c.questions, slot)
			}
		}
	}
	if len(c.exam.Sections) == 0 {
		return nil, parser.Permanent("ir_empty", fmt.Errorf("no questions were extracted"))
	}
	c.deterministicMerge()
	return c, nil
}

// deterministicMerge applies the auditable rules: adjacent-page continuation
// with an empty display number joins its head; answer entries with an
// explicit answer_for bind to the unique question with that display number.
// Anything uncertain stays unbound with a warning.
func (c *candidate) deterministicMerge() {
	displayCount := map[string]int{}
	for i := range c.questions {
		s := &c.questions[i]
		if s.out.AnswerFor == "" && c.item(s).DisplayNumber != "" {
			displayCount[c.item(s).DisplayNumber]++
		}
	}
	for i := range c.questions {
		s := &c.questions[i]
		if s.removed {
			continue
		}
		// Answer binding: explicit number, unique target.
		if s.out.AnswerFor != "" {
			target := c.uniqueByDisplay(s.out.AnswerFor, displayCount)
			if target != nil && !target.removed && target != s {
				c.bindAnswer(target, s)
				continue
			}
			c.warnings = append(c.warnings, fmt.Sprintf("answer entry %s for %q could not be bound deterministically", s.ref, s.out.AnswerFor))
			continue
		}
		// Continuation: empty display number, previous slot marked continues.
		if c.item(s).DisplayNumber == "" && i > 0 {
			prev := &c.questions[i-1]
			if !prev.removed && prev.out.Continues {
				c.joinQuestions(prev, s)
				continue
			}
		}
	}
	c.dropRemoved()
}

func (c *candidate) uniqueByDisplay(num string, counts map[string]int) *questionSlot {
	if counts[num] != 1 {
		return nil
	}
	for i := range c.questions {
		s := &c.questions[i]
		if !s.removed && s.out.AnswerFor == "" && c.item(s).DisplayNumber == num {
			return s
		}
	}
	return nil
}

// joinQuestions appends the tail's content to the head and drops the tail.
func (c *candidate) joinQuestions(head, tail *questionSlot) {
	hq, tq := c.item(head), c.item(tail)
	hq.Stem = append(hq.Stem, tq.Stem...)
	hq.Options = append(hq.Options, tq.Options...)
	hq.Analysis = append(hq.Analysis, tq.Analysis...)
	if hq.Answer == nil {
		hq.Answer = tq.Answer
	}
	head.out.Continues = tail.out.Continues
	tail.removed = true
}

// bindAnswer moves an answer entry's answer/analysis onto its question and
// drops the entry.
func (c *candidate) bindAnswer(target, entry *questionSlot) {
	tq, eq := c.item(target), c.item(entry)
	if eq.Answer != nil {
		if tq.Answer == nil {
			tq.Answer = eq.Answer
		} else {
			c.warnings = append(c.warnings, fmt.Sprintf("question %s already has an answer; entry %s kept unbound", target.ref, entry.ref))
			return
		}
	}
	if len(eq.Analysis) > 0 && len(tq.Analysis) == 0 {
		tq.Analysis = eq.Analysis
	}
	entry.removed = true
}

// dropRemoved rebuilds each section's question list from the surviving slots
// and updates their positions, so later accessors stay correct.
func (c *candidate) dropRemoved() {
	for si := range c.exam.Sections {
		var kept []ir.QuestionItem
		for i := range c.questions {
			s := &c.questions[i]
			if s.section != si || s.removed {
				continue
			}
			kept = append(kept, c.exam.Sections[si].Questions[s.position])
			s.position = len(kept) - 1
		}
		c.exam.Sections[si].Questions = kept
	}
	// A section whose questions all merged away is dropped.
	var sections []ir.Section
	remap := map[int]int{}
	for si, sec := range c.exam.Sections {
		if len(sec.Questions) == 0 {
			continue
		}
		remap[si] = len(sections)
		sections = append(sections, sec)
	}
	c.exam.Sections = sections
	for i := range c.questions {
		s := &c.questions[i]
		if ns, ok := remap[s.section]; ok {
			s.section = ns
		}
	}
}

// ambiguous reports whether unresolved relations remain that only a semantic
// text merge could decide: a continues head with no joined tail, or an
// unbound answer entry.
func (c *candidate) ambiguous() (mergeInput, bool) {
	in := mergeInput{}
	need := false
	for i := range c.questions {
		s := &c.questions[i]
		if s.removed {
			continue
		}
		q := c.item(s)
		mq := mergeQuestion{Ref: s.ref, Page: s.page, DisplayNumber: q.DisplayNumber,
			StemText: boundedText(blocksText(q.Stem), 2000), Continues: s.out.Continues}
		in.Questions = append(in.Questions, mq)
		if s.out.Continues {
			need = true
		}
		if s.out.AnswerFor != "" || (q.DisplayNumber == "" && q.Answer != nil) {
			need = true
			in.Answers = append(in.Answers, mergeAnswer{Ref: s.ref, Page: s.page,
				Values: answerValues(q.Answer), Analysis: boundedText(blocksText(q.Analysis), 2000)})
		}
	}
	return in, need
}

// applyMergePatch applies the allowlisted joins and answer binds. Unknown
// refs, self-joins and joins onto removed slots are ignored with a warning;
// nothing else in the document may change.
func (c *candidate) applyMergePatch(out mergeOutput) {
	byRef := map[string]*questionSlot{}
	for i := range c.questions {
		byRef[c.questions[i].ref] = &c.questions[i]
	}
	for _, j := range out.Joins {
		head, tail := byRef[j.HeadRef], byRef[j.TailRef]
		if head == nil || tail == nil || head == tail || head.removed || tail.removed {
			c.warnings = append(c.warnings, fmt.Sprintf("merge join %s->%s ignored: unknown or invalid refs", j.HeadRef, j.TailRef))
			continue
		}
		c.joinQuestions(head, tail)
	}
	for _, b := range out.AnswerBinds {
		entry, target := byRef[b.AnswerRef], byRef[b.QuestionRef]
		if entry == nil || target == nil || entry == target || entry.removed || target.removed {
			c.warnings = append(c.warnings, fmt.Sprintf("merge bind %s->%s ignored: unknown or invalid refs", b.AnswerRef, b.QuestionRef))
			continue
		}
		c.bindAnswer(target, entry)
	}
	c.dropRemoved()
}

func blocksText(blocks []ir.ContentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		switch block.Type {
		case "text":
			b.WriteString(block.Markdown)
			b.WriteByte(' ')
		case "math_block":
			b.WriteString(block.LaTeX)
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func answerValues(a *ir.Answer) []string {
	if a == nil {
		return nil
	}
	return a.ShortValues
}

func boundedText(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}

func toIRQuestion(out questionOut) (*ir.QuestionItem, error) {
	q := &ir.QuestionItem{
		QuestionID:    out.QuestionID,
		Kind:          out.Kind,
		DisplayNumber: out.DisplayNumber,
		QuestionType:  out.QuestionType,
		Score:         out.Score,
		AnswerSpaceMM: out.AnswerSpaceMM,
	}
	var err error
	if q.Stem, err = toIRBlocks(out.Stem); err != nil {
		return nil, err
	}
	for _, opt := range out.Options {
		content, err := toIRBlocks(opt.Content)
		if err != nil {
			return nil, err
		}
		q.Options = append(q.Options, ir.Option{Label: opt.Label, Content: content})
	}
	if out.Answer != nil {
		detail, err := toIRBlocks(out.Answer.Detail)
		if err != nil {
			return nil, err
		}
		q.Answer = &ir.Answer{ShortValues: out.Answer.ShortValues, Detail: detail}
	}
	if q.Analysis, err = toIRBlocks(out.Analysis); err != nil {
		return nil, err
	}
	for _, child := range out.Children {
		cq, err := toIRQuestion(child)
		if err != nil {
			return nil, err
		}
		q.Children = append(q.Children, *cq)
	}
	return q, nil
}

func toIRBlocks(blocks []blockOut) ([]ir.ContentBlock, error) {
	var out []ir.ContentBlock
	for _, b := range blocks {
		cb := ir.ContentBlock{Type: b.Type, Markdown: b.Markdown, LaTeX: b.LaTeX, LayoutHint: b.LayoutHint, Table: b.Table}
		if b.Type == "image_ref" {
			// The model key is carried in AssetID until the server remap; it
			// is never a persistence ID.
			cb.AssetID = b.AssetKey
		}
		out = append(out, cb)
	}
	return out, nil
}

func toIRBBox(b *bboxOut) (*ir.BBox, error) {
	if b == nil || len(b.Rect) != 4 {
		return nil, fmt.Errorf("bbox needs four coordinates")
	}
	return &ir.BBox{Page: b.Page, Rect: [4]float64{b.Rect[0], b.Rect[1], b.Rect[2], b.Rect[3]}}, nil
}

// sortedPages orders extractions by page number, rejecting duplicates.
func sortedPages(pages []pageExtraction) ([]pageExtraction, error) {
	out := append([]pageExtraction(nil), pages...)
	sort.Slice(out, func(i, j int) bool { return out[i].Page < out[j].Page })
	for i := 1; i < len(out); i++ {
		if out[i].Page == out[i-1].Page {
			return nil, parser.Permanent("vlm_protocol", fmt.Errorf("duplicate extraction for page %d", out[i].Page))
		}
	}
	return out, nil
}
