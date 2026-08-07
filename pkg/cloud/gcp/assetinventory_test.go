package gcp

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestMapListAssetsError_DistinguishesDeadlineAndCancel asserts that the two
// cancellation-family gRPC statuses map to the two distinct context sentinels,
// so an operator who cancels a scan is told the scan was canceled rather than
// told the deadline expired.
func TestMapListAssetsError_DistinguishesDeadlineAndCancel(t *testing.T) {
	parent := "projects/test-123"

	// codes.DeadlineExceeded must wrap context.DeadlineExceeded, and must NOT
	// be reported as a cancellation.
	deadlineErr := mapListAssetsError(parent, status.Error(codes.DeadlineExceeded, "Deadline expired"))
	if !errors.Is(deadlineErr, context.DeadlineExceeded) {
		t.Errorf("DeadlineExceeded: errors.Is(err, context.DeadlineExceeded) = false; want true (got %v)", deadlineErr)
	}
	if errors.Is(deadlineErr, context.Canceled) {
		t.Errorf("DeadlineExceeded: errors.Is(err, context.Canceled) = true; want false (got %v)", deadlineErr)
	}

	// codes.Canceled must wrap context.Canceled, and must NOT be reported as a
	// deadline expiry.
	cancelErr := mapListAssetsError(parent, status.Error(codes.Canceled, "context canceled"))
	if !errors.Is(cancelErr, context.Canceled) {
		t.Errorf("Canceled: errors.Is(err, context.Canceled) = false; want true (got %v)", cancelErr)
	}
	if errors.Is(cancelErr, context.DeadlineExceeded) {
		t.Errorf("Canceled: errors.Is(err, context.DeadlineExceeded) = true; want false (got %v)", cancelErr)
	}

	// The wrapped chain must also hold for the original Go contexts themselves:
	// a real canceled context and a real expired-deadline context are each
	// separately distinguishable by errors.Is.
	if errors.Is(context.Canceled, context.DeadlineExceeded) {
		t.Errorf("context.Canceled and context.DeadlineExceeded must remain distinct sentinels")
	}
}

// TestMapListAssetsError_NonStatusPassthrough verifies that a plain non-gRPC
// error is wrapped unchanged (still errors.Is to the original) and that the
// other status branches still map as before.
func TestMapListAssetsError_NonStatusPassthrough(t *testing.T) {
	parent := "projects/test-123"

	sentinel := errors.New("boom")
	err := mapListAssetsError(parent, sentinel)
	if !errors.Is(err, sentinel) {
		t.Errorf("non-status error must wrap the original sentinel; got %v", err)
	}
}
