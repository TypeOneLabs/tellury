// Package cli is the command surface: flag parsing, logging setup and the one
// place where ingestion, enrichment, rules and rendering meet.
package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	_ "github.com/TypeOneLabs/tellury/pkg/rules/all" // register built-in rules
)

// Exit codes — a stable CI contract.
const (
	ExitOK       = 0 // ran clean, no findings
	ExitError    = 1 // operational failure
	ExitUsage    = 2 // bad flags
	ExitFindings = 3 // ran clean, findings present (gate builds on this)
)

// globalFlags are the persistent flags shared by every subcommand.
type globalFlags struct {
	LogLevel string
	NoColor  bool
	Timeout  time.Duration
}

// errFindings signals a clean run that found waste. It carries no message: the
// table is the message.
var errFindings = errors.New("findings present")

// usageError marks a flag/validation failure so Execute can return ExitUsage.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func newUsageError(err error) error {
	if err == nil {
		return nil
	}
	return usageError{err: err}
}

// Execute builds the command tree, runs it and maps the outcome to an exit code.
func Execute() (int, error) {
	var g globalFlags

	root := &cobra.Command{
		Use:           "tellury",
		Short:         "Find and price cloud waste. Zero bloat.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.LogLevel, "log-level", "warn", "error|warn|info|debug")
	pf.BoolVar(&g.NoColor, "no-color", false, "disable ANSI color")
	pf.DurationVar(&g.Timeout, "timeout", 5*time.Minute, "overall deadline")

	root.AddCommand(
		newScanCmd(&g),
		newRulesCmd(&g),
		newGraphCmd(&g),
		newVersionCmd(),
	)

	ctx, cancel := signalContext(context.Background())
	defer cancel()

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK, nil
	}
	if errors.Is(err, errFindings) {
		return ExitFindings, nil
	}
	var ue usageError
	if errors.As(err, &ue) {
		return ExitUsage, err
	}
	return ExitError, err
}

// signalContext cancels on SIGINT/SIGTERM so a long scan stops promptly.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// newLogger builds the diagnostic logger. It writes to stderr, always: stdout
// belongs to the report, so `tellury scan --format json | jq` is never polluted.
func newLogger(g *globalFlags, w io.Writer) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(g.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelWarn
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// withTimeout applies the global deadline.
func withTimeout(ctx context.Context, g *globalFlags) (context.Context, context.CancelFunc) {
	if g.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, g.Timeout)
}
