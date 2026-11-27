// Package pipeline is the offline-testable ingest pipeline: transactional
// submission of a River job with its ingest_jobs row, the worker state
// machine, and the classification of permanent versus transient failures.
//
// River owns the retry schedule. The domain row (ingest_jobs.state) records
// what happened; it never duplicates the queue.
package pipeline

import (
	"errors"
	"fmt"

	"examforge/internal/store"
)

// Domain transitions. Everything else is illegal.
var transitions = map[store.JobState][]store.JobState{
	store.JobQueued:     {store.JobParsing, store.JobFailed},
	store.JobParsing:    {store.JobValidating, store.JobFailed},
	store.JobValidating: {store.JobPersisted, store.JobFailed},
	store.JobPersisted:  nil,
	store.JobFailed:     nil,
}

// ClaimStates are the states a worker start may claim from: the first run
// (queued) and re-entry after an attempt that died in an active state without
// recording an outcome (parsing, validating).
var ClaimStates = []store.JobState{store.JobQueued, store.JobParsing, store.JobValidating}

// CanTransition reports whether from -> to is a legal edge.
func CanTransition(from, to store.JobState) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Terminal reports whether a state is an outcome. A terminal job is never
// re-run; a worker that meets one returns an idempotent result.
func Terminal(state store.JobState) bool {
	return state == store.JobPersisted || state == store.JobFailed
}

// IsState reports whether s is one of the defined states.
func IsState(s store.JobState) bool {
	_, ok := transitions[s]
	return ok
}

var errIllegalTransition = errors.New("pipeline: illegal ingest state transition")

// CheckTransition validates a state move, including the worker-start re-entry
// into parsing, and reports the offending edge.
func CheckTransition(from, to store.JobState) error {
	if !IsState(from) || !IsState(to) {
		return fmt.Errorf("%w: %q -> %q is not a defined state", errIllegalTransition, from, to)
	}
	if to == store.JobParsing {
		for _, claimable := range ClaimStates {
			if from == claimable {
				return nil
			}
		}
	}
	if CanTransition(from, to) {
		return nil
	}
	return fmt.Errorf("%w: %q -> %q", errIllegalTransition, from, to)
}
