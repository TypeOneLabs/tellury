package aws

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRunFetchJobs_IsolatesPerJobFailures verifies that one failing job does
// not cancel healthy siblings.
func TestRunFetchJobs_IsolatesPerJobFailures(t *testing.T) {
	var completed atomic.Int64
	worker := func(ctx context.Context, j fetchJob) error {
		if j.account == "poisoned" {
			return fmt.Errorf("account %s unreachable", j.account)
		}
		completed.Add(1)
		return nil
	}

	var jobs []fetchJob
	for i := 0; i < 20; i++ {
		jobs = append(jobs, fetchJob{key: "k", account: fmt.Sprintf("a%02d", i), region: "us-east-1"})
	}
	jobs = append(jobs, fetchJob{key: "k", account: "poisoned", region: "us-east-1"})

	err := runFetchJobs(context.Background(), jobs, worker)
	if err == nil {
		t.Fatal("runFetchJobs must report the poisoned job's failure")
	}
	if got := completed.Load(); got != 20 {
		t.Fatalf("healthy jobs ran %d times; want all 20 to run (errgroup cancellation regression)", got)
	}
}

// TestRunFetchJobs_ProgressReceivesCumulativeDone pins the progress seam:
// onDone is invoked once per job with cumulative completed count.
func TestRunFetchJobs_ProgressReceivesCumulativeDone(t *testing.T) {
	const njobs = 15
	worker := func(context.Context, fetchJob) error { return nil }

	var (
		mu  sync.Mutex
		got []int
	)
	onDone := func(done int) {
		mu.Lock()
		got = append(got, done)
		mu.Unlock()
	}

	var jobs []fetchJob
	for i := 0; i < njobs; i++ {
		jobs = append(jobs, fetchJob{key: "k", account: "a", region: "us-east-1"})
	}
	if err := runFetchJobs(context.Background(), jobs, worker, onDone); err != nil {
		t.Fatalf("runFetchJobs: %v", err)
	}

	sort.Ints(got)
	if len(got) != njobs {
		t.Fatalf("progress callback invoked %d times, want %d", len(got), njobs)
	}
	for i, d := range got {
		if d != i+1 {
			t.Fatalf("cumulative done counts out of order: got[%d]=%d, want %d", i, d, i+1)
		}
	}
}
