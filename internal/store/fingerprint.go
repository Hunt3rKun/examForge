package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"examforge/internal/ir"
)

// FingerprintVersion is the canonicalization algorithm version. Changing the
// canonical value or its encoding requires a new version and a backfill; the
// hash meaning of an existing version never changes silently.
const FingerprintVersion = 1

// fingerprintExcludedMetadata lists metadata keys that are classification or
// dedup hints rather than question semantics. The list is fixed: knowledge
// points and difficulty do not enter the fingerprint, and the upstream
// dedup_fingerprint is untrusted and ignored.
var fingerprintExcludedMetadata = map[string]bool{
	"knowledge_points":  true,
	"difficulty":        true,
	"dedup_fingerprint": true,
}

var errFingerprint = errors.New("store: question cannot be fingerprinted")

// canonicalQuestion is the fingerprint input: question semantics only. It
// leaves out question_id (entity IDs at any depth), display_number and score
// (per-exam occurrence values) and provenance (source/review context).
type canonicalQuestion struct {
	Kind          string              `json:"kind"`
	QuestionType  string              `json:"question_type,omitempty"`
	AnswerSpaceMM float64             `json:"answer_space_mm,omitempty"`
	Stem          []ir.ContentBlock   `json:"stem"`
	Options       []ir.Option         `json:"options,omitempty"`
	Answer        *ir.Answer          `json:"answer,omitempty"`
	Analysis      []ir.ContentBlock   `json:"analysis,omitempty"`
	Children      []canonicalQuestion `json:"children,omitempty"`
	Metadata      map[string]any      `json:"metadata,omitempty"`
}

// QuestionFingerprint returns the deterministic SHA-256 of a question's
// semantic content. Equal fingerprints mean equal semantics; they do not mean
// one entity, because display number, score and provenance may differ.
func QuestionFingerprint(q ir.QuestionItem) (string, error) {
	canonical, err := canonicalize(q)
	if err != nil {
		return "", err
	}
	// encoding/json is deterministic here: struct fields are in declared
	// order and map keys are sorted; non-finite numbers are rejected above.
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errFingerprint, err)
	}
	sum := sha256.Sum256(append([]byte("examforge/question/v1\x00"), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

func canonicalize(q ir.QuestionItem) (canonicalQuestion, error) {
	out := canonicalQuestion{
		Kind:          q.Kind,
		QuestionType:  q.QuestionType,
		AnswerSpaceMM: normalizeFloat(q.AnswerSpaceMM),
		Stem:          blocksOrNil(q.Stem),
		Options:       q.Options,
		Answer:        q.Answer,
		Analysis:      blocksOrNil(q.Analysis),
	}
	if len(q.Metadata) > 0 {
		out.Metadata = make(map[string]any, len(q.Metadata))
		for key, value := range q.Metadata {
			if !fingerprintExcludedMetadata[key] {
				out.Metadata[key] = value
			}
		}
		if len(out.Metadata) == 0 {
			out.Metadata = nil
		}
	}
	if err := checkJSONValue(".metadata", out.Metadata); err != nil {
		return out, err
	}
	if len(q.Children) > 0 {
		out.Children = make([]canonicalQuestion, 0, len(q.Children))
		for i, child := range q.Children {
			c, err := canonicalize(child)
			if err != nil {
				return out, fmt.Errorf("children[%d]: %w", i, err)
			}
			out.Children = append(out.Children, c)
		}
	}
	return out, nil
}

// checkJSONValue rejects non-finite numbers, which encoding/json refuses.
func checkJSONValue(path string, value any) error {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: %s is not a finite number", errFingerprint, path)
		}
	case map[string]any:
		for key, item := range v {
			if err := checkJSONValue(path+"."+key, item); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range v {
			if err := checkJSONValue(fmt.Sprintf("%s[%d]", path, i), item); err != nil {
				return err
			}
		}
	}
	return nil
}

// normalizeFloat gives -0 and 0 one encoding.
func normalizeFloat(f float64) float64 {
	if f == 0 {
		return 0
	}
	return f
}

// blocksOrNil gives an absent and an empty block list one encoding.
func blocksOrNil(blocks []ir.ContentBlock) []ir.ContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	return blocks
}
