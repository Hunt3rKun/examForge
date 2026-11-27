package store

import (
	"context"

	"examforge/internal/ir"
	"examforge/internal/store/sqlcgen"
)

// Persister runs the assembler inside one repository transaction, so the
// exam, its sections and items, and the job completion commit together.
type Persister struct {
	repo      *Repository
	assembler *Assembler
}

// NewPersister binds the assembler to the repository.
func NewPersister(repo *Repository, assembler *Assembler) (*Persister, error) {
	if repo == nil || assembler == nil {
		return nil, ErrInvalid
	}
	return &Persister{repo: repo, assembler: assembler}, nil
}

// Persist validates and writes the exam in one transaction.
func (p *Persister) Persist(ctx context.Context, exam ir.ExamDocumentIR, source SourceRecord) (rec ExamRecord, err error) {
	err = p.repo.InTx(ctx, func(ctx context.Context, ws *sqlcgen.Queries) error {
		var err error
		rec, err = p.assembler.Persist(ctx, ws, exam, source)
		return err
	})
	if err != nil {
		return ExamRecord{}, Translate(err)
	}
	return rec, nil
}
