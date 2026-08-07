package compiler

import (
	"context"
	"fmt"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// NativeCompiler resolves an ID against the built-in registry.
//
// It exists so `tellury` can treat native and compiled rules through one
// Compiler interface, and so a future `--rule-file custom.yaml` path composes
// with built-ins in a single code path.
type NativeCompiler struct{}

var _ Compiler = NativeCompiler{}

// Name implements Compiler.
func (NativeCompiler) Name() string { return "native" }

// Compile resolves Source.Text (or Source.Spec.ID) as a registered rule ID.
func (NativeCompiler) Compile(_ context.Context, src Source) (rules.Rule, []Diagnostic, error) {
	id := strings.TrimSpace(src.Text)
	if src.Spec != nil && src.Spec.ID != "" {
		id = src.Spec.ID
	}
	if id == "" {
		return nil, nil, fmt.Errorf("%w: no rule ID given", ErrUnknownRule)
	}
	r, ok := rules.Get(id)
	if !ok {
		return nil, nil, fmt.Errorf("%w %q", ErrUnknownRule, id)
	}
	return r, []Diagnostic{{
		Severity: DiagInfo,
		Message:  fmt.Sprintf("resolved built-in rule %q", id),
	}}, nil
}

// Chain tries each compiler in order and returns the first success. It is how
// the CLI will resolve a mixed --rules/--rule-file selection without branching
// on the source of every rule.
type Chain []Compiler

var _ Compiler = Chain(nil)

// Name implements Compiler.
func (c Chain) Name() string {
	names := make([]string, 0, len(c))
	for _, comp := range c {
		names = append(names, comp.Name())
	}
	return "chain(" + strings.Join(names, ",") + ")"
}

// Compile implements Compiler.
func (c Chain) Compile(ctx context.Context, src Source) (rules.Rule, []Diagnostic, error) {
	var diags []Diagnostic
	var lastErr error
	for _, comp := range c {
		r, d, err := comp.Compile(ctx, src)
		diags = append(diags, d...)
		if err == nil {
			return r, diags, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: empty compiler chain", ErrInvalidSpec)
	}
	return nil, diags, lastErr
}
