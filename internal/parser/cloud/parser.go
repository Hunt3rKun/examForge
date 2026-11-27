package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"

	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/vision/raster"
)

// CloudAPIParser converts a dirty scanned exam PDF into a validated candidate
// Exam IR plus raw/clean asset payloads. It performs no uploads, no database
// writes and no queue operations.
type CloudAPIParser struct {
	cfg       Config
	transport *transport
	extractor PageExtractor
	merger    TextMerger
	repairer  *openAIMerger
}

// NewCloudAPIParser validates the configuration and wires the protocol.
func NewCloudAPIParser(cfg Config) (*CloudAPIParser, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	t := newTransport(cfg)
	m := &openAIMerger{t: t}
	p := &CloudAPIParser{
		cfg:       cfg,
		transport: t,
		extractor: &openAIExtractor{t: t},
		merger:    m,
		repairer:  m,
	}
	if cfg.Extractor != nil {
		p.extractor = cfg.Extractor
	}
	if cfg.Merger != nil {
		p.merger = cfg.Merger
	}
	return p, nil
}

// Parse runs the fixed stage order: bounded read → raster → transfer copies →
// concurrent page extraction → deterministic merge (+ optional text merge) →
// server ID remap → full validation with bounded repair → crop and clean →
// metadata backfill → final read-only validation.
func (p *CloudAPIParser) Parse(ctx context.Context, source parser.Source) (parser.ParseResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.TotalParseTimeout)
	defer cancel()
	diag := Diagnostics{}
	u := &usage{}
	defer func() {
		diag.PromptTokens, diag.CompletionTokens = int(u.prompt.Load()), int(u.completion.Load())
		if p.cfg.Diagnostics != nil {
			p.cfg.Diagnostics(diag)
		}
	}()

	data, err := parser.ReadSource(ctx, source)
	if err != nil {
		return parser.ParseResult{}, err
	}

	// Stage: rasterize every page at high resolution.
	t0 := time.Now()
	rasterCtx, rasterCancel := context.WithTimeout(ctx, p.cfg.RasterTimeout)
	pages, err := p.cfg.Rasterizer.Render(rasterCtx, data, raster.PageRange{})
	rasterCancel()
	diag.RasterMillis = time.Since(t0).Milliseconds()
	if err != nil {
		return parser.ParseResult{}, classifyLocal(err, "raster_failed")
	}
	diag.Pages = len(pages)
	pageByNumber := make(map[int]raster.Page, len(pages))
	for _, page := range pages {
		pageByNumber[page.Number] = page
	}

	// Stage: bounded transfer copies. The high-resolution originals stay
	// local for the final crop.
	t0 = time.Now()
	transfers := make([][]byte, len(pages))
	for i, page := range pages {
		jpegBytes, err := transferJPEG(page.Image, p.cfg.TransportLongEdge, p.cfg.JPEGQuality, p.cfg.MaxJPEGBytes)
		if err != nil {
			return parser.ParseResult{}, err
		}
		transfers[i] = jpegBytes
	}

	// Stage: concurrent per-page extraction into page-indexed slots, so the
	// merge order never depends on network completion order.
	extractions := make([]pageExtraction, len(pages))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(p.cfg.PageConcurrency)
	for i, page := range pages {
		i, page := i, page
		group.Go(func() error {
			if err := p.budgetCheck(ctx); err != nil {
				return err
			}
			out, err := p.extractor.ExtractPage(groupCtx, page.Number, transfers[i], u)
			if err != nil {
				return err
			}
			if out.Page != 0 && out.Page != page.Number {
				return parser.Permanent("vlm_protocol", fmt.Errorf("extraction claims page %d for request page %d", out.Page, page.Number))
			}
			out.Page = page.Number
			extractions[i] = out
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return parser.ParseResult{}, err
	}
	diag.ExtractMillis = time.Since(t0).Milliseconds()

	sorted, err := sortedPages(extractions)
	if err != nil {
		return parser.ParseResult{}, err
	}

	// Stage: deterministic merge, then the optional text-only merge.
	t0 = time.Now()
	cand, err := mergePages(sorted)
	if err != nil {
		return parser.ParseResult{}, err
	}
	if input, need := cand.ambiguous(); need {
		if err := p.budgetCheck(ctx); err != nil {
			return parser.ParseResult{}, err
		}
		diag.MergeCalls++
		out, err := p.merger.MergeText(ctx, input, u)
		if err != nil {
			return parser.ParseResult{}, err
		}
		cand.applyMergePatch(out)
	}
	diag.MergeMillis = time.Since(t0).Milliseconds()

	// Stage: server-side ID remap before any validation.
	if err := cand.remapAssets(p.cfg.NewAssetID, p.cfg.NewExamID()); err != nil {
		return parser.ParseResult{}, err
	}
	cand.exam.Metadata.SourceSHA256 = source.SHA256

	// Stage: full validation with bounded repair rounds.
	t0 = time.Now()
	exam, err := p.validateWithRepair(ctx, cand.exam, len(pages), u, &diag)
	diag.RepairMillis = time.Since(t0).Milliseconds()
	if err != nil {
		return parser.ParseResult{}, err
	}

	// Read-only revalidation passed; only now touch pixels.
	t0 = time.Now()
	result, err := p.cropAndClean(ctx, exam, cand.assets, pageByNumber)
	diag.CropMillis = time.Since(t0).Milliseconds()
	if err != nil {
		return parser.ParseResult{}, err
	}
	diag.Warnings = len(result.Exam.Warnings)

	// Final read-only validation. A failure here is an implementation/input
	// consistency defect, never another LLM round.
	if errs := p.fullValidation(result.Exam, len(pages)); len(errs) > 0 {
		return parser.ParseResult{}, parser.Permanent("final_validation", fmt.Errorf("post-crop validation failed: %v", errs))
	}
	return result, nil
}

// budgetCheck refuses to start an external call when the remaining total
// budget cannot cover it plus the worker reserve.
func (p *CloudAPIParser) budgetCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	if time.Until(deadline) <= p.cfg.WorkerReserve {
		return parser.Transient("budget_exhausted", fmt.Errorf("parse budget has less than the %v worker reserve left", p.cfg.WorkerReserve))
	}
	return nil
}

// fullValidation runs the complete IR validator plus the bbox constraints.
func (p *CloudAPIParser) fullValidation(exam ir.ExamDocumentIR, pageCount int) []string {
	var errs []string
	if err := p.cfg.Validator.ValidateExamDTO(exam); err != nil {
		errs = append(errs, err.Error())
	}
	errs = append(errs, validateBBoxes(exam, pageCount)...)
	return errs
}

// validateWithRepair runs full validation and at most MaxRepairRounds bounded
// repair requests. Protected asset identity is enforced on every repaired
// candidate before it is validated again.
func (p *CloudAPIParser) validateWithRepair(ctx context.Context, exam ir.ExamDocumentIR, pageCount int, u *usage, diag *Diagnostics) (ir.ExamDocumentIR, error) {
	errs := p.fullValidation(exam, pageCount)
	if len(errs) == 0 {
		return exam, nil
	}
	protected := snapshotProtected(exam)
	for round := 0; round < p.cfg.MaxRepairRounds; round++ {
		if err := p.budgetCheck(ctx); err != nil {
			return ir.ExamDocumentIR{}, err
		}
		candidateJSON, err := json.Marshal(exam)
		if err != nil {
			return ir.ExamDocumentIR{}, parser.Permanent("ir_invalid", err)
		}
		diag.RepairRounds++
		raw, err := p.repairer.repair(ctx, candidateJSON, errs, u)
		if err != nil {
			return ir.ExamDocumentIR{}, err
		}
		var repaired ir.ExamDocumentIR
		if err := json.Unmarshal(raw, &repaired); err != nil {
			return ir.ExamDocumentIR{}, parser.Permanent("vlm_protocol", fmt.Errorf("repair candidate decode: %w", err))
		}
		if err := protected.enforceProtected(repaired); err != nil {
			return ir.ExamDocumentIR{}, parser.Permanent("repair_protected", err)
		}
		// Server-owned metadata is never taken from the repair response.
		repaired.ExamID = exam.ExamID
		repaired.Metadata.SourceSHA256 = exam.Metadata.SourceSHA256
		exam = repaired
		errs = p.fullValidation(exam, pageCount)
		if len(errs) == 0 {
			return exam, nil
		}
	}
	return ir.ExamDocumentIR{}, parser.Permanent("repair_exhausted", fmt.Errorf("candidate still invalid after %d repair rounds: %v", p.cfg.MaxRepairRounds, errs))
}

// cropAndClean cuts every asset from its high-resolution source page and
// attempts cleaning. A cleaner failure falls back to raw_cropped with a
// warning; a crop/decode failure is a permanent parse failure naming the
// asset and page.
func (p *CloudAPIParser) cropAndClean(ctx context.Context, exam ir.ExamDocumentIR, decls map[string]assetOut, pages map[int]raster.Page) (parser.ParseResult, error) {
	result := parser.ParseResult{Exam: exam}
	ids := make([]string, 0, len(exam.Assets))
	for id := range exam.Assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		asset := exam.Assets[id]
		if err := ctx.Err(); err != nil {
			return parser.ParseResult{}, err
		}
		page, ok := pages[asset.SourceBBox.Page]
		if !ok {
			return parser.ParseResult{}, parser.Permanent("crop_page_missing", fmt.Errorf("asset %q names page %d which was not rasterized", id, asset.SourceBBox.Page))
		}
		crop, err := raster.CropPNG(page.Image, asset.SourceBBox.Rect, page.DPI, p.cfg.CropConfig)
		if err != nil {
			return parser.ParseResult{}, parser.Permanent("crop_failed", fmt.Errorf("asset %q page %d: %w", id, asset.SourceBBox.Page, err))
		}
		blob := parser.AssetBlob{
			AssetID:   id,
			MediaType: asset.MediaType,
			RawData:   crop.PNG,
			RawSHA256: crop.SHA256,
		}
		asset.AspectRatio = crop.AspectRatio
		asset.SuggestedWidthMM = crop.SuggestedWidthMM
		cleaned, cleanErr := p.cfg.Cleaner.Clean(ctx, bytes.NewReader(crop.PNG))
		switch {
		case cleanErr != nil:
			if ctx.Err() != nil {
				return parser.ParseResult{}, ctx.Err()
			}
			asset.Status = "raw_cropped"
			result.Exam.Warnings = append(result.Exam.Warnings, boundedText(fmt.Sprintf("asset %s: cleaner failed, kept raw crop", id), 512))
		case bytes.Equal(cleaned.PNG, crop.PNG):
			// Nothing changed: storing identical bytes twice pretends a
			// cleanup happened. Keep the honest raw result.
			asset.Status = "raw_cropped"
		default:
			asset.Status = "cleaned"
			asset.RawURI = assetURIScheme + id + "/raw"
			blob.CleanData = cleaned.PNG
			blob.SHA256 = cleaned.SHA256
			for _, w := range cleaned.Warnings {
				result.Exam.Warnings = append(result.Exam.Warnings, boundedText("asset "+id+": "+w, 512))
			}
		}
		result.Exam.Assets[id] = asset
		result.Assets = append(result.Assets, blob)
	}
	return result, nil
}

// classifyLocal maps a local raster failure: context problems stay context
// errors (River retries), everything else is a permanent input defect.
func classifyLocal(err error, code string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return parser.Permanent(code, err)
}
