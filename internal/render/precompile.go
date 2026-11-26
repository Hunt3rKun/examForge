package render

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"examforge/internal/ir"
	"examforge/internal/ir/inline"
)

// blockUse says where a block is rendered. Only stems group a float_right image
// with the paragraphs beside it; elsewhere the hint cannot apply.
type blockUse int

const (
	useStem blockUse = iota
	useDetail
	useOption
)

// Precompile builds the measure-pass RenderPayload for one exam and layout.
// It tokenizes every inline text field, normalizes tables and float groups,
// resolves layout constants and lists the referenced assets. It does no I/O
// and never mutates exam. The renderer then copies the assets, runs the
// measure pass and fills in the resolved option grids.
func Precompile(exam ir.ExamDocumentIR, layout ir.LayoutSpec) (*RenderPayload, error) {
	if err := validateLayout(layout); err != nil {
		return nil, err
	}
	lay, err := newRenderLayout(layout)
	if err != nil {
		return nil, err
	}
	c := &precompiler{exam: exam, lay: lay, assets: map[string]*ResolvedAsset{}}

	lay.Header, lay.Footer = nil, nil
	if res := inline.Parse(layout.HeaderText); len(res.Tokens) > 0 {
		c.reportInline(res.Diagnostics, "layout.header_text")
		lay.Header = &RenderInline{Tokens: res.Tokens}
	}
	if res := inline.Parse(layout.FooterText); len(res.Tokens) > 0 {
		c.reportInline(res.Diagnostics, "layout.footer_text")
		lay.Footer = &RenderInline{Tokens: res.Tokens}
	}

	payload := &RenderPayload{
		Version: PayloadVersion,
		Pass:    PassMeasure,
		Exam: RenderExam{
			ExamID:    exam.ExamID,
			Title:     exam.Metadata.Title,
			Subtitle:  exam.Metadata.Subtitle,
			Questions: []RenderQuestion{},
		},
		Layout: lay,
		Assets: []ResolvedAsset{},
	}
	for i, q := range exam.Questions {
		rq, err := c.question(q, fmt.Sprintf("questions[%d]", i))
		if err != nil {
			return nil, err
		}
		payload.Exam.Questions = append(payload.Exam.Questions, rq)
	}
	payload.Assets = c.assetList()
	payload.Warnings = c.warnings
	if payload.Warnings == nil {
		payload.Warnings = []RenderWarning{}
	}
	return payload, nil
}

type precompiler struct {
	exam     ir.ExamDocumentIR
	lay      RenderLayout
	assets   map[string]*ResolvedAsset
	warnings []RenderWarning
	next     int
}

func (c *precompiler) warn(code, path, format string, args ...any) {
	c.warnings = append(c.warnings, RenderWarning{Code: code, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (c *precompiler) reportInline(diags []inline.Diagnostic, path string) {
	for _, dg := range diags {
		c.warn("inline_"+dg.Code, path, "%s (bytes %d-%d)", dg.Message, dg.StartByte, dg.EndByte)
	}
}

func (c *precompiler) tokenize(text, path string) []inline.Token {
	res := inline.Parse(text)
	c.reportInline(res.Diagnostics, path)
	return res.Tokens
}

func (c *precompiler) question(q ir.QuestionItem, path string) (RenderQuestion, error) {
	if math.IsNaN(q.AnswerSpaceMM) || math.IsInf(q.AnswerSpaceMM, 0) || q.AnswerSpaceMM < 0 {
		return RenderQuestion{}, fmt.Errorf("%w: %s.answer_space_mm must be a finite non-negative number, got %v", ErrInvalidInput, path, q.AnswerSpaceMM)
	}
	out := RenderQuestion{
		Index:           c.next,
		Kind:            q.Kind,
		DisplayNumber:   q.DisplayNumber,
		Stem:            []RenderBlock{},
		AnswerSpaceMM:   q.AnswerSpaceMM,
		OptionRequested: c.lay.OptionLayout,
		OptionGrid:      c.lay.OptionLayout,
		OptionColumns:   GridColumns(c.lay.OptionLayout),
	}
	c.next++

	var err error
	if out.Stem, err = c.blocks(useStem, q.Stem, path+".stem"); err != nil {
		return RenderQuestion{}, err
	}
	if len(q.Options) > 0 {
		out.Options = make([]RenderOption, 0, len(q.Options))
		for i, opt := range q.Options {
			op := fmt.Sprintf("%s.options[%d]", path, i)
			content, err := c.blocks(useOption, opt.Content, op+".content")
			if err != nil {
				return RenderQuestion{}, err
			}
			out.Options = append(out.Options, RenderOption{
				Label:   RenderInline{Tokens: c.tokenize(opt.Label, op+".label")},
				Content: content,
			})
		}
	}
	if c.lay.ShowAnswer && q.Answer != nil {
		// show_answer and show_analysis are independent switches: neither is
		// implied or forced by the other, and mode never overrides them.
		out.Answer = &RenderAnswer{}
		for i, v := range q.Answer.ShortValues {
			out.Answer.ShortValues = append(out.Answer.ShortValues, RenderInline{Tokens: c.tokenize(v, fmt.Sprintf("%s.answer.short_values[%d]", path, i))})
		}
		if out.Answer.Detail, err = c.blocks(useDetail, q.Answer.Detail, path+".answer.detail"); err != nil {
			return RenderQuestion{}, err
		}
	}
	if c.lay.ShowAnalysis && len(q.Analysis) > 0 {
		if out.Analysis, err = c.blocks(useDetail, q.Analysis, path+".analysis"); err != nil {
			return RenderQuestion{}, err
		}
	}
	// Answer space is reserved for a blank student answer area only: teacher
	// mode stays compact, and a student paper that shows answers lays the
	// answer out instead of reserving the same space again.
	if c.lay.Mode == ModeStudent && !c.lay.ShowAnswer && q.AnswerSpaceMM > 0 {
		out.ReserveSpaceMM = q.AnswerSpaceMM
	}
	for i, child := range q.Children {
		rc, err := c.question(child, fmt.Sprintf("%s.children[%d]", path, i))
		if err != nil {
			return RenderQuestion{}, err
		}
		out.Children = append(out.Children, rc)
	}
	return out, nil
}

// assetList returns every referenced asset once, sorted by asset_id so the
// payload is deterministic. Paths are workspace-relative and derived from the
// validated ID alone.
func (c *precompiler) assetList() []ResolvedAsset {
	out := make([]ResolvedAsset, 0, len(c.assets))
	for _, a := range c.assets {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID < out[j].AssetID })
	return out
}

// blocks converts IR content blocks in document order.
func (c *precompiler) blocks(use blockUse, in []ir.ContentBlock, path string) ([]RenderBlock, error) {
	out := make([]RenderBlock, 0, len(in))
	for i := 0; i < len(in); i++ {
		b := in[i]
		bp := fmt.Sprintf("%s[%d]", path, i)
		if b.Type == "image_ref" && b.LayoutHint == "float_right" {
			if use == useStem {
				// Local side-by-side: the image narrows the adjacent paragraphs
				// up to the next non-text block. It never leaves this stem.
				j := i + 1
				for j < len(in) && in[j].Type == "text" {
					j++
				}
				if j > i+1 {
					group := make([]RenderBlock, 0, j-i-1)
					for k := i + 1; k < j; k++ {
						blk, err := c.block(useStem, in[k], fmt.Sprintf("%s[%d]", path, k))
						if err != nil {
							return nil, err
						}
						if blk.Kind != "" {
							group = append(group, blk)
						}
					}
					if len(group) > 0 {
						img, err := c.image(b, bp, c.lay.MaxFloatWidthMM)
						if err != nil {
							return nil, err
						}
						out = append(out, RenderBlock{Kind: BlockFloatGroup, AssetID: img.AssetID, Src: img.Src, WidthMM: img.WidthMM, Blocks: group})
						i = j - 1
						continue
					}
				}
			}
			// Unsafe to float: a right-aligned block image, and the fallback is visible.
			img, err := c.block(use, b, bp)
			if err != nil {
				return nil, err
			}
			if use == useStem {
				img.Align = "right"
				c.warn("float_fallback", bp, "float_right image has no adjacent text block in this stem; rendered as a right-aligned block image")
			} else {
				c.warn("float_not_applicable", bp, "layout_hint float_right is only honored in question stems; rendered as a plain image")
			}
			out = append(out, img)
			continue
		}
		blk, err := c.block(use, b, bp)
		if err != nil {
			return nil, err
		}
		if blk.Kind != "" {
			out = append(out, blk)
		}
	}
	return out, nil
}

// block converts one content block. An empty RenderBlock means "nothing to render".
func (c *precompiler) block(use blockUse, b ir.ContentBlock, path string) (RenderBlock, error) {
	switch b.Type {
	case "text":
		tokens := c.tokenize(b.Markdown, path)
		if len(tokens) == 0 {
			return RenderBlock{}, nil
		}
		return RenderBlock{Kind: BlockParagraph, Tokens: tokens}, nil
	case "math_block":
		// Display formula: the LaTeX is payload, never inline-parsed.
		if b.LaTeX == "" {
			return RenderBlock{}, nil
		}
		return RenderBlock{Kind: BlockMath, LaTeX: b.LaTeX}, nil
	case "image_ref":
		maxMM := c.lay.MaxImageWidthMM
		if use == useOption {
			maxMM = c.lay.MaxOptionImageMM
		}
		img, err := c.image(b, path, maxMM)
		if err != nil {
			return RenderBlock{}, err
		}
		if use != useOption {
			img.Align = "center"
		}
		return img, nil
	case "table":
		t, err := c.table(b.Table, path)
		if err != nil {
			return RenderBlock{}, err
		}
		return RenderBlock{Kind: BlockTable, Table: t}, nil
	case "":
		return RenderBlock{}, nil
	}
	// Unknown block: keep the kind and raw fields, render a placeholder and
	// warn. It is never silently dropped.
	raw, err := json.Marshal(b)
	if err != nil {
		raw = nil
	}
	c.warn("unknown_block", path, "content block of type %q is not supported by this renderer; rendered as a placeholder", b.Type)
	return RenderBlock{Kind: BlockUnknown, Source: b.Type, Raw: raw}, nil
}

func (c *precompiler) image(b ir.ContentBlock, path string, maxMM float64) (RenderBlock, error) {
	if err := validateAssetID(b.AssetID); err != nil {
		return RenderBlock{}, fmt.Errorf("%w: %s: %v", ErrInvalidInput, path, err)
	}
	asset, ok := c.exam.Assets[b.AssetID]
	if !ok {
		return RenderBlock{}, fmt.Errorf("%w: %s references asset %q which is not declared in exam.assets", ErrInvalidInput, path, b.AssetID)
	}
	ext, err := imageExt(asset.MediaType)
	if err != nil {
		return RenderBlock{}, fmt.Errorf("%w: %s references asset %q: %v", ErrInvalidInput, path, b.AssetID, err)
	}
	width := asset.SuggestedWidthMM
	if width <= 0 || math.IsNaN(width) || math.IsInf(width, 0) {
		width = c.lay.DefaultImageWidthMM
	}
	if width > maxMM {
		width = maxMM
	}
	staged, seen := c.assets[b.AssetID]
	if !seen {
		staged = &ResolvedAsset{
			AssetID:   b.AssetID,
			Path:      WorkspaceAssets + "/" + b.AssetID + "." + ext,
			MediaType: asset.MediaType,
			WidthMM:   clampWidth(asset.SuggestedWidthMM, c.lay),
		}
		c.assets[b.AssetID] = staged
	}
	return RenderBlock{Kind: BlockImage, AssetID: b.AssetID, Src: staged.Path, WidthMM: width}, nil
}

func clampWidth(suggested float64, lay RenderLayout) float64 {
	if suggested <= 0 || math.IsNaN(suggested) || math.IsInf(suggested, 0) {
		suggested = lay.DefaultImageWidthMM
	}
	if suggested > lay.MaxImageWidthMM {
		suggested = lay.MaxImageWidthMM
	}
	return suggested
}

// table normalizes the IR table into a rectangular grid of tokenized cells.
func (c *precompiler) table(t *ir.Table, path string) (*RenderTable, error) {
	if t == nil {
		return nil, fmt.Errorf("%w: %s is a table block without table data", ErrInvalidInput, path)
	}
	grid, err := buildTableGrid(*t)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalidInput, path, err)
	}
	out := &RenderTable{Columns: grid.Columns, Rows: make([][]RenderCell, len(grid.Rows))}
	for r, row := range grid.Rows {
		out.Rows[r] = make([]RenderCell, 0, len(row))
		for i, cell := range row {
			out.Rows[r] = append(out.Rows[r], RenderCell{
				Tokens:  c.tokenize(cell.Text, fmt.Sprintf("%s.table[%d][%d]", path, r, i)),
				ColSpan: cell.ColSpan,
				RowSpan: cell.RowSpan,
			})
		}
	}
	return out, nil
}
