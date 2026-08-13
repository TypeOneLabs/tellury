package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// unsetNO_COLOR ensures a test runs with NO_COLOR absent, restoring whatever
// the host environment had when the test finishes.
func unsetNO_COLOR(t *testing.T) {
	t.Helper()
	old, had := os.LookupEnv("NO_COLOR")
	if !had {
		return
	}
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("NO_COLOR", old) })
}

// TestColorEnabledDisablePrecedence pins the off switch, highest precedence
// first. The terminal gate is the last rule and a bytes.Buffer is not a TTY,
// so every row below resolves false; the rows are ordered by the precedence
// implemented in colorEnabled, and each one exercises its own short-circuit.
func TestColorEnabledDisablePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		noColor    bool
		noColorEnv bool
		term       string
		want       bool
	}{
		{
			name:    "no-color is the explicit per-invocation override",
			noColor: true,
			term:    "xterm-256color",
			want:    false,
		},
		{
			name:       "NO_COLOR present, even empty, disables colour",
			noColorEnv: true,
			term:       "xterm-256color",
			want:       false,
		},
		{
			name: "TERM=dumb disables colour",
			term: "dumb",
			want: false,
		},
		{
			name: "non-TTY stdout is the final gate",
			term: "xterm-256color",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColorEnv {
				t.Setenv("NO_COLOR", "")
			} else {
				unsetNO_COLOR(t)
			}
			t.Setenv("TERM", tc.term)

			if got := colorEnabled(&bytes.Buffer{}, tc.noColor); got != tc.want {
				t.Fatalf("colorEnabled(buffer, noColor=%v) = %v, want %v", tc.noColor, got, tc.want)
			}
		})
	}
}

// TestColorEnabled_NonTerminalFileIsFalse confirms a real file is treated the
// same as a pipe or buffer: colour never reaches a non-character-device.
func TestColorEnabled_NonTerminalFileIsFalse(t *testing.T) {
	unsetNO_COLOR(t)
	t.Setenv("TERM", "xterm-256color")

	f, err := os.CreateTemp(t.TempDir(), "tellury-colour")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if colorEnabled(f, false) {
		t.Fatal("colorEnabled must be false for a regular file (non-TTY)")
	}
}

// TestNoColorFlagHelpText pins the flag's new meaning: --no-color is now read
// by the CLI, so its help text must say what it does instead of claiming it is
// ignored.
func TestNoColorFlagHelpText(t *testing.T) {
	_, _, stdout, _ := runExecute(t, "--help")
	if !strings.Contains(stdout, "disable ANSI colour in terminal output") {
		t.Errorf("--no-color help text must describe disabling ANSI colour:\n%s", stdout)
	}
	if strings.Contains(stdout, "accepted and ignored") {
		t.Errorf("--no-color help text must no longer claim the flag is ignored:\n%s", stdout)
	}
}
