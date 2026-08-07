package gcp

import (
	"fmt"
	"os"
)

// readFile wraps os.ReadFile with a GCP-package-specific error prefix. It is
// used by StaticPricer.OverlayFile so the package does not need the parent
// package's overlay helper; keep every I/O seam local to the GCP package.
func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pricing: read price file %q: %w", path, err)
	}
	return data, nil
}
