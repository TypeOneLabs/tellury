package aws

import (
	"github.com/TypeOneLabs/tellury/pkg/metrics"
)

// Window resolves the effective lookback for an AWS spec: a spec-fixed window
// always wins over the request's --window. Specs whose WindowDays is 0 follow
// the request's window.
func Window(req metrics.Request, spec Spec) int {
	if spec.WindowDays > 0 {
		return spec.WindowDays
	}
	return req.WindowDays
}
