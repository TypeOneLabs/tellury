// Package compiler turns declarative rule specifications into executable
// rules.Rule values.
//
// The design refuses to embed an LLM in the CLI. Instead there is a two-stage
// pipeline with a declarative IR in the middle:
//
//	natural language ──Frontend──► RuleSpec (IR) ──Interpret──► rules.Rule
//
// The IR is the contract. Who writes it — a human today, a model tomorrow — is
// irrelevant to the engine, and a generated rule is reviewable as data before it
// is ever allowed to price anything.
package compiler

import (
	"context"
	"errors"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// Compiler errors.
var (
	ErrNoFrontend  = errors.New("compiler: Source.Spec is nil and no Frontend is configured")
	ErrInvalidSpec = errors.New("compiler: rule spec is invalid")
	ErrUnknownRule = errors.New("compiler: unknown rule ID")
)

// Source is an input to be compiled into a Rule.
type Source struct {
	// Text is a natural-language intent, e.g. "flag persistent disks that no VM
	// has used for more than 30 days".
	Text string
	// Spec is a pre-authored IR. When non-nil, Text is documentation only and no
	// frontend is invoked. This is the path this build supports end to end.
	Spec *RuleSpec
	// Hints narrow the search space for a natural-language frontend.
	Hints Hints
}

// Hints narrow the search space for an NL frontend.
type Hints struct {
	Provider string
	Service  string
	Kinds    []string
}

// Diagnostic severities.
const (
	DiagError   = "error"
	DiagWarning = "warning"
	DiagInfo    = "info"
)

// Diagnostic is a compiler message. Non-fatal warnings still yield a Rule.
type Diagnostic struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
	// Path is a JSON pointer into the RuleSpec, e.g. "/where/0/field".
	Path string `json:"path,omitempty"`
}

func errorDiag(path, msg string) Diagnostic {
	return Diagnostic{Severity: DiagError, Message: msg, Path: path}
}

func warnDiag(path, msg string) Diagnostic {
	return Diagnostic{Severity: DiagWarning, Message: msg, Path: path}
}

// HasError reports whether any diagnostic is fatal.
func HasError(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.Severity == DiagError {
			return true
		}
	}
	return false
}

// Compiler turns a Source into an executable Rule.
//
// Implementations MUST be deterministic for a given Source when Spec != nil.
// An NL frontend MUST surface the intermediate RuleSpec so a human can review
// generated logic before it prices anything.
type Compiler interface {
	Name() string
	Compile(ctx context.Context, src Source) (rules.Rule, []Diagnostic, error)
}

// Frontend is the swappable NL→IR stage. This build ships no implementation;
// the interface is the seam an LLM or DSL frontend plugs into later, with the
// whole downstream pipeline already built and tested.
type Frontend interface {
	Translate(ctx context.Context, text string, h Hints) (*RuleSpec, []Diagnostic, error)
}

// Pipeline is the reference Compiler: optional Frontend → Validate → Interpret.
type Pipeline struct {
	FE Frontend // may be nil ⇒ Source.Spec is required
}

var _ Compiler = Pipeline{}

// Name implements Compiler.
func (p Pipeline) Name() string { return "spec-pipeline" }

// Compile implements Compiler.
func (p Pipeline) Compile(ctx context.Context, src Source) (rules.Rule, []Diagnostic, error) {
	spec := src.Spec
	var diags []Diagnostic

	if spec == nil {
		if p.FE == nil {
			return nil, nil, ErrNoFrontend
		}
		translated, d, err := p.FE.Translate(ctx, src.Text, src.Hints)
		diags = append(diags, d...)
		if err != nil {
			return nil, diags, err
		}
		spec = translated
	}

	diags = append(diags, spec.Validate()...)
	if HasError(diags) {
		return nil, diags, ErrInvalidSpec
	}

	r, err := Interpret(spec)
	return r, diags, err
}
