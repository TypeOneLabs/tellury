package gcp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// TestRunFetchJobs_IsolatesPerJobFailures is the regression test for finding
// #3 (per-job isolation): one failing (key,project) pair must not cancel the
// healthy siblings. The errgroup.WithContext implementation cancelled every
// sibling on the first error; the bounded pool keeps them all running so the
// healthy jobs still write their data.
func TestRunFetchJobs_IsolatesPerJobFailures(t *testing.T) {
	var setCalls int32
	worker := func(ctx context.Context, j fetchJob) error {
		if j.project == "poisoned" {
			return errors.New("project poisoned: no monitoring.viewer")
		}
		atomic.AddInt32(&setCalls, 1)
		return nil
	}

	var jobs []fetchJob
	for i := 0; i < 50; i++ {
		jobs = append(jobs, fetchJob{key: "k", project: fmt.Sprintf("p%02d", i)})
	}
	jobs = append(jobs, fetchJob{key: "k", project: "poisoned"})

	err := runFetchJobs(context.Background(), jobs, worker)
	if err == nil {
		t.Fatalf("runFetchJobs must report the poisoned job's failure")
	}
	if got := atomic.LoadInt32(&setCalls); got != 50 {
		t.Fatalf("healthy jobs ran %d times; want all 50 to run (errgroup cancellation regression)", got)
	}
}

// TestRunFetchJobs_NoCancelOnSiblingErrRegression directly proves the
// sibling-cancellation is gone: five failing jobs are followed by one healthy
// job that must still run.
func TestRunFetchJobs_NoCancelOnSiblingErrRegression(t *testing.T) {
	var (
		failed     atomic.Int32
		sawHealthy atomic.Bool
		runsSeen   atomic.Int32
	)

	worker := func(ctx context.Context, j fetchJob) error {
		runsSeen.Add(1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if j.project == "bad" {
			failed.Add(1)
			return errors.New("bad project")
		}
		sawHealthy.Store(true)
		return nil
	}

	var jobs []fetchJob
	for i := 0; i < 10; i++ {
		jobs = append(jobs, fetchJob{key: "k", project: "bad"})
	}
	jobs = append(jobs, fetchJob{key: "k", project: "healthy"})

	err := runFetchJobs(context.Background(), jobs, worker)
	if err == nil {
		t.Fatalf("expected joined error from 10 bad jobs")
	}
	if !sawHealthy.Load() {
		t.Fatalf("healthy job was cancelled by sibling failures; must still run in isolation")
	}
	if got := failed.Load(); got != 10 {
		t.Fatalf("expected 10 bad jobs to fail, got %d", got)
	}
	// Every job must have run: 10 failing siblings plus the healthy one. If
	// any goroutine was skipped from scheduling, the pool contract is broken.
	if got := runsSeen.Load(); got != 11 {
		t.Fatalf("expected all 11 jobs to run, got %d", got)
	}
}

// TestRunFetchJobs_JoinedErrorsInspectable guards against a regression where
// runFetchJobs chained multiple failures with fmt.Errorf("%w; %v", ...): that
// keeps reachable only the FIRST error through errors.Is/errors.As and
// flattens the rest to text. With errors.Join every individual failure — not
// just the first — must stay inspectable.
func TestRunFetchJobs_JoinedErrorsInspectable(t *testing.T) {
	var (
		errFirst            = errors.New("second-job sentinel failure")
		errLast             = errors.New("last-job sentinel failure")
		failFirst, failLast bool
	)
	worker := func(_ context.Context, j fetchJob) error {
		switch {
		case j.project == "first":
			failFirst = true
			return errors.New("first job ordinary failure")
		case j.project == "second":
			failLast = true
			return errFirst
		case j.project == "third":
			return errLast
		default:
			return nil
		}
	}

	jobs := []fetchJob{
		{key: "k", project: "first"},
		{key: "k", project: "second"},
		{key: "k", project: "third"},
		{key: "k", project: "ok"},
	}

	err := runFetchJobs(context.Background(), jobs, worker)
	if err == nil {
		t.Fatalf("runFetchJobs must report the multi-job failure")
	}

	// A sentinel from the SECOND failing job must remain reachable. Under the
	// old %w;%v chaining this was flattened to text and errors.Is missed it.
	if !errors.Is(err, errFirst) {
		t.Fatalf("errors.Is(err, secondJobSentinel) = false; want true — every joined error must stay inspectable")
	}
	// And a sentinel from the LAST failing job too, not just the first.
	if !errors.Is(err, errLast) {
		t.Fatalf("errors.Is(err, lastJobSentinel) = false; want true — every joined error must stay inspectable")
	}
	if !failFirst || !failLast {
		t.Fatalf("worker side-effects missing: failFirst=%v failLast=%v", failFirst, failLast)
	}
}
