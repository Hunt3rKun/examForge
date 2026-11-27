package pipeline

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"examforge/internal/parser"
	"examforge/internal/storage"
	"examforge/internal/store"
)

func TestStateMachineAcceptsOnlyDocumentedEdges(t *testing.T) {
	legal := [][2]store.JobState{
		{store.JobQueued, store.JobParsing},
		{store.JobParsing, store.JobValidating},
		{store.JobValidating, store.JobPersisted},
		{store.JobQueued, store.JobFailed},
		{store.JobParsing, store.JobFailed},
		{store.JobValidating, store.JobFailed},
		// A worker re-enters parsing after an attempt died in an active state.
		{store.JobParsing, store.JobParsing},
		{store.JobValidating, store.JobParsing},
	}
	for _, edge := range legal {
		if err := CheckTransition(edge[0], edge[1]); err != nil {
			t.Errorf("%s -> %s rejected: %v", edge[0], edge[1], err)
		}
	}
	illegal := [][2]store.JobState{
		{store.JobQueued, store.JobValidating},
		{store.JobQueued, store.JobPersisted},
		{store.JobParsing, store.JobQueued},
		{store.JobParsing, store.JobPersisted},
		{store.JobValidating, store.JobQueued},
		{store.JobPersisted, store.JobParsing},
		{store.JobPersisted, store.JobFailed},
		{store.JobFailed, store.JobParsing},
		{store.JobFailed, store.JobFailed},
		{"retrying", store.JobParsing},
	}
	for _, edge := range illegal {
		if err := CheckTransition(edge[0], edge[1]); err == nil {
			t.Errorf("%s -> %s accepted", edge[0], edge[1])
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		permanent bool
		code      string
	}{
		{"permanent wrapper", Permanent(CodeIRInvalid, errors.New("bad")), true, CodeIRInvalid},
		{"parser permanent", parser.Permanent("not_a_pdf", errors.New("bad")), true, CodeParserPrefix + "not_a_pdf"},
		{"parser transient", parser.Transient("gateway", errors.New("timeout")), false, CodeTransient},
		{"store invalid", store.ErrInvalid, true, CodeStoreInvalid},
		{"store conflict", store.ErrConflict, true, CodeStoreConflict},
		{"asset conflict", storage.ErrConflict, true, CodeAssetConflict},
		{"object missing", storage.ErrNotFound, true, CodeObjectMissing},
		{"object forbidden", storage.ErrForbidden, true, CodeObjectInvalid},
		{"object transient", storage.ErrUnavailable, false, CodeTransient},
		{"unknown", errors.New("network reset"), false, CodeTransient},
	}
	for _, c := range cases {
		code, permanent := Classify(c.err, CodeTransient)
		if permanent != c.permanent || code != c.code {
			t.Errorf("%s: Classify = (%s, %v), want (%s, %v)", c.name, code, permanent, c.code, c.permanent)
		}
	}
	// A wrapped cause keeps the store class.
	if code, permanent := Classify(fmt.Errorf("persist: %w", store.ErrConflict), CodeTransient); !permanent || code != CodeStoreConflict {
		t.Errorf("wrapped store error = (%s, %v)", code, permanent)
	}
}

func TestRedactionKeepsMessagesUsable(t *testing.T) {
	msg := storage.Redact("connect to http://ak:sk@host:9000 failed", 200, "sk")
	if strings.Contains(msg, "ak:sk") || strings.Contains(msg, "host:9000") {
		t.Errorf("redacted message = %q", msg)
	}
	if !strings.Contains(msg, "failed") {
		t.Errorf("redacted message lost its meaning: %q", msg)
	}
}
