package cli

import (
	"io"
	"os"
	"strings"
)

// colorEnabled resolves whether ANSI colour may be written to out. It is the
// single stdout-colour gate for the human table renderer, and it applies the
// disable precedence in order:
//
//  1. --no-color is the explicit, per-invocation user intent.
//  2. NO_COLOR present in the environment disables colour; presence matters,
//     not value, so a set-but-empty NO_COLOR still disables.
//  3. TERM=dumb disables colour even on a character device.
//  4. Finally, colour requires stdout to be a terminal, reusing the existing
//     isTerminal helper from progress.go — the one "is this a TTY" answer in
//     this codebase.
//
// JSON and CSV never call this: those renderers have no colour field and no
// colour code path.
func colorEnabled(out io.Writer, noColor bool) bool {
	if noColor {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return isTerminal(out)
}
