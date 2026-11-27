package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"examforge/internal/store"
	"examforge/internal/store/storetest"
)

// fakeRiver records inserted and cancelled jobs. InsertTx runs inside the
// caller's transaction scope; if the scope fails afterwards the test asserts
// that the fake transaction rolled back.
type fakeRiver struct {
	mu        sync.Mutex
	nextID    int64
	inserted  []fakeJob
	cancelled []int64
	insertErr error
	cancelErr error
}

type fakeJob struct {
	ID   int64
	Args river.JobArgs
	Opts *river.InsertOpts
}

func (f *fakeRiver) InsertTx(_ context.Context, _ pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	f.nextID++
	f.inserted = append(f.inserted, fakeJob{ID: f.nextID, Args: args, Opts: opts})
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{ID: f.nextID}}, nil
}

func (f *fakeRiver) JobCancel(_ context.Context, jobID int64) (*rivertype.JobRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	f.cancelled = append(f.cancelled, jobID)
	return &rivertype.JobRow{ID: jobID, State: rivertype.JobStateCancelled}, nil
}

func (f *fakeRiver) insertedJobs() []fakeJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeJob(nil), f.inserted...)
}

func (f *fakeRiver) cancelledJobs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.cancelled...)
}

// memRunner runs submission transactions against the in-memory store, with
// rollback on error. It also serves the worker's repository calls through
// repoShim.
type memRunner struct {
	db *storetest.MemDB
}

func (r memRunner) Run(ctx context.Context, fn func(ctx context.Context, scope TxScope) error) error {
	return r.db.InTx(ctx, func(ctx context.Context) error {
		return fn(ctx, memScope(r))
	})
}

type memScope struct{ db *storetest.MemDB }

func (s memScope) Jobs() store.IngestJobs { return s.db }
func (s memScope) Raw() pgx.Tx            { return nil } // the fake River ignores it

func sourceDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
