package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"
)

type eventStopper struct {
	events *[]string
	err    error
}

func (s eventStopper) Stop(context.Context) error {
	*s.events = append(*s.events, "river")
	return s.err
}

type eventCloser struct{ events *[]string }

func (c eventCloser) Close() { *c.events = append(*c.events, "pool") }

func TestLifecycleShutdownOrderAndIdempotency(t *testing.T) {
	var events []string
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	shutdown := &lifecycle{ready: func(v bool) {
		if !v {
			events = append(events, "not-ready")
		}
	}, http: srv, river: eventStopper{events: &events}, pool: eventCloser{events: &events}, timeout: time.Second}
	if err := shutdown.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := shutdown.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"not-ready", "river", "pool"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("shutdown events = %v, want %v", events, want)
	}
}

func TestLifecycleAggregatesErrorsAndStillClosesPool(t *testing.T) {
	var events []string
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopErr := errors.New("river stop failed")
	l := &lifecycle{ready: func(bool) { events = append(events, "not-ready") }, http: &http.Server{}, river: eventStopper{events: &events, err: stopErr}, pool: eventCloser{events: &events}, timeout: time.Second}
	err := l.shutdown(ctx)
	if err == nil || !errors.Is(err, stopErr) {
		t.Fatalf("expected aggregated river error, got %v", err)
	}
	if len(events) != 3 || events[0] != "not-ready" || events[1] != "river" || events[2] != "pool" {
		t.Fatalf("cleanup skipped or reordered: %v", events)
	}
}
