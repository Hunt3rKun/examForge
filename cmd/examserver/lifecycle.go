package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type stopper interface{ Stop(context.Context) error }
type closer interface{ Close() }

type lifecycle struct {
	ready   func(bool)
	http    *http.Server
	river   stopper
	pool    closer
	timeout time.Duration
	once    sync.Once
	err     error
}

// shutdown lowers readiness first and always attempts every cleanup phase.
// Concurrent or repeated calls share the single result and do not close twice.
func (l *lifecycle) shutdown(parent context.Context) error {
	l.once.Do(func() {
		if l.ready != nil {
			l.ready(false)
		}
		ctx, cancel := context.WithTimeout(parent, l.timeout)
		defer cancel()
		var errs []error
		if l.http != nil {
			if err := l.http.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("HTTP shutdown: %w", err))
				if closeErr := l.http.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
					errs = append(errs, fmt.Errorf("HTTP close: %w", closeErr))
				}
			}
		}
		if l.river != nil {
			if err := l.river.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("river stop: %w", err))
			}
		}
		if l.pool != nil {
			l.pool.Close()
		}
		l.err = errors.Join(errs...)
	})
	return l.err
}
