package cloud

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"

	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/storage"
)

// assetURIScheme mirrors store.AssetURIScheme. It is duplicated as a constant
// because importing internal/store here would pull pgx and River into the
// parser's import graph; the value is part of the exchange contract.
const assetURIScheme = "examforge-asset://"

func defaultAssetID() string { return "ast_" + strings.ReplaceAll(uuid.NewString(), "-", "") }
func defaultExamID() string  { return "exam_" + strings.ReplaceAll(uuid.NewString(), "-", "") }

// remapAssets replaces every model-local asset key with a server-generated
// global ID in one pass: the assets map keys, each asset_id, and every nested
// image_ref. Undefined references, duplicate keys and ID collisions are
// permanent input errors. The model never decides persistence identity.
func (c *candidate) remapAssets(newID func() string, examID string) error {
	keys := make([]string, 0, len(c.assets))
	for key := range c.assets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	mapping := make(map[string]string, len(keys))
	used := map[string]bool{}
	referenced := map[string]bool{}
	collectRefs(&c.exam, referenced)
	for _, key := range keys {
		if _, ok := mapping[key]; ok {
			return parser.Permanent("ir_asset_invalid", fmt.Errorf("duplicate asset key %q", key))
		}
		id := newID()
		if err := storage.ValidateAssetID(id); err != nil {
			return parser.Permanent("ir_asset_invalid", fmt.Errorf("generated asset ID: %w", err))
		}
		if used[id] {
			return parser.Permanent("ir_asset_invalid", fmt.Errorf("generated asset ID %q collides", id))
		}
		used[id] = true
		mapping[key] = id
	}
	for ref := range referenced {
		if _, ok := mapping[ref]; !ok {
			return parser.Permanent("ir_asset_dangling", fmt.Errorf("image_ref %q has no declared asset", ref))
		}
	}
	for key := range c.assets {
		if !referenced[key] {
			return parser.Permanent("ir_asset_unreferenced", fmt.Errorf("asset %q is declared but never referenced", key))
		}
	}
	assets := make(map[string]ir.Asset, len(keys))
	for _, key := range keys {
		decl := c.assets[key]
		id := mapping[key]
		bbox, err := toIRBBox(decl.BBox)
		if err != nil {
			return parser.Permanent("ir_bbox_invalid", fmt.Errorf("asset %q: %w", key, err))
		}
		mediaType := decl.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		assets[id] = ir.Asset{
			AssetID:    id,
			MediaType:  mediaType,
			URI:        assetURIScheme + id,
			Status:     "raw_cropped", // provisional; the crop stage sets the real result
			Caption:    decl.Caption,
			Alt:        decl.Alt,
			SourceBBox: bbox,
		}
	}
	c.exam.Assets = assets
	c.exam.ExamID = examID
	rewriteRefs(&c.exam, mapping)
	// Keep the declaration map keyed by the new IDs for the crop stage.
	remapped := make(map[string]assetOut, len(keys))
	for _, key := range keys {
		remapped[mapping[key]] = c.assets[key]
	}
	c.assets = remapped
	return nil
}

// collectRefs gathers every image_ref asset key at any depth.
func collectRefs(exam *ir.ExamDocumentIR, into map[string]bool) {
	walkBlocks(exam, func(blocks []ir.ContentBlock) {
		for _, b := range blocks {
			if b.Type == "image_ref" {
				into[b.AssetID] = true
			}
		}
	})
}

// rewriteRefs applies the mapping to every image_ref in place.
func rewriteRefs(exam *ir.ExamDocumentIR, mapping map[string]string) {
	walkBlocks(exam, func(blocks []ir.ContentBlock) {
		for i := range blocks {
			if blocks[i].Type == "image_ref" {
				blocks[i].AssetID = mapping[blocks[i].AssetID]
			}
		}
	})
}

// walkBlocks visits every mutable block slice in the document.
func walkBlocks(exam *ir.ExamDocumentIR, fn func([]ir.ContentBlock)) {
	var walkQuestion func(q *ir.QuestionItem)
	walkQuestion = func(q *ir.QuestionItem) {
		fn(q.Stem)
		for i := range q.Options {
			fn(q.Options[i].Content)
		}
		if q.Answer != nil {
			fn(q.Answer.Detail)
		}
		fn(q.Analysis)
		for i := range q.Children {
			walkQuestion(&q.Children[i])
		}
	}
	for si := range exam.Sections {
		for qi := range exam.Sections[si].Questions {
			walkQuestion(&exam.Sections[si].Questions[qi])
		}
	}
}

// validateBBoxes runs the slice-3 source-document constraints on top of the
// IR validator: every asset needs a finite, correctly ordered bbox whose page
// exists in the rasterized source. Errors carry the asset ID and page.
func validateBBoxes(exam ir.ExamDocumentIR, pageCount int) []string {
	var errs []string
	ids := make([]string, 0, len(exam.Assets))
	for id := range exam.Assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		asset := exam.Assets[id]
		path := fmt.Sprintf("assets[%q].source_bbox", id)
		if asset.SourceBBox == nil {
			errs = append(errs, fmt.Sprintf("%s is required", path))
			continue
		}
		bb := asset.SourceBBox
		if bb.Page < 1 || bb.Page > pageCount {
			errs = append(errs, fmt.Sprintf("%s.page %d is outside 1..%d", path, bb.Page, pageCount))
		}
		for i, v := range bb.Rect {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
				errs = append(errs, fmt.Sprintf("%s.rect[%d] = %v is outside [0,1]", path, i, v))
			}
		}
		if bb.Rect[0] >= bb.Rect[2] {
			errs = append(errs, fmt.Sprintf("%s.rect x1 %v must be < x2 %v", path, bb.Rect[0], bb.Rect[2]))
		}
		if bb.Rect[1] >= bb.Rect[3] {
			errs = append(errs, fmt.Sprintf("%s.rect y1 %v must be < y2 %v", path, bb.Rect[1], bb.Rect[3]))
		}
	}
	return errs
}

// protectedAssets snapshots the fields a repair response may never change.
type protectedAssets struct {
	ids    map[string]bool
	bboxes map[string]ir.BBox
	status map[string]string
}

func snapshotProtected(exam ir.ExamDocumentIR) protectedAssets {
	p := protectedAssets{ids: map[string]bool{}, bboxes: map[string]ir.BBox{}, status: map[string]string{}}
	for id, asset := range exam.Assets {
		p.ids[id] = true
		p.status[id] = asset.Status
		if asset.SourceBBox != nil {
			p.bboxes[id] = *asset.SourceBBox
		}
	}
	return p
}

// enforceProtected checks a repaired candidate against the snapshot: the
// asset set must be identical and every server ID unchanged. BBox geometry
// may be repaired (that is the point), but a bbox may not be dropped.
func (p protectedAssets) enforceProtected(exam ir.ExamDocumentIR) error {
	if len(exam.Assets) != len(p.ids) {
		return fmt.Errorf("repair changed the asset count from %d to %d", len(p.ids), len(exam.Assets))
	}
	for id, asset := range exam.Assets {
		if !p.ids[id] {
			return fmt.Errorf("repair introduced asset %q", id)
		}
		if asset.AssetID != id {
			return fmt.Errorf("repair changed asset_id of %q to %q", id, asset.AssetID)
		}
		if _, hadBBox := p.bboxes[id]; hadBBox && asset.SourceBBox == nil {
			return fmt.Errorf("repair removed source_bbox of asset %q", id)
		}
		if !p.ids[asset.AssetID] {
			return fmt.Errorf("repair rebound asset %q", id)
		}
	}
	// Every image_ref must still name a protected ID.
	refs := map[string]bool{}
	collectRefs(&exam, refs)
	for ref := range refs {
		if !p.ids[ref] {
			return fmt.Errorf("repair introduced image_ref to unknown asset %q", ref)
		}
	}
	return nil
}
