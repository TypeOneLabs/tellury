# Working in this repository

Notes for a coding agent, and for anyone who wants the conventions in one place. Nothing
here is tool-specific.

## Build and test

```bash
go build ./...
go vet ./...
go test ./...
```

All three must pass before a change is done. `go test -race ./...` needs cgo and a C
compiler; it is not required for a normal change.

## What this is

A CLI that reads cloud inventory, builds an in-memory resource graph, and evaluates
deterministic rules against it to find and price waste. GCP, AWS and Azure. See the
[README](README.md) for the shape of it and [docs/](docs/) for reference material.

## Adding a rule

Read **[docs/writing-a-rule.md](docs/writing-a-rule.md)** first, all of it. It walks the
`NodeRule` interface method by method, the typed skip-code vocabulary, registration, and
the mutation check, with `old_snapshot` as a worked example that is real code in this
repository.

Three things that repeatedly go wrong, stated here so they are hard to miss:

**Registration is silent when it fails.** A rule registers itself from `init()`, so a rule
in a package nothing imports registers as nothing at all — and build, vet and tests all
stay green. Add the blank import to `pkg/rules/all/all.go` and confirm the rule appears in
`tellury rules list`.

**A test that passes against broken logic is worse than no test**, because it looks
verified. Before you are done: break the condition your rule detects, run the test, watch
it FAIL, then restore it. Report the failure you saw. This project has shipped several
tests that asserted nothing, and every one of them looked fine in review.

**Never invent a value you did not read.** Attribute names, metric keys, SKU tokens and API
category fields must be copied from the code or from a live response, never guessed from
what seems reasonable. A SKU matcher was once written against a plausible-looking resource
family that the real catalogue does not use; it passed its test, matched nothing in
production, and silently fell back to a stale price table for months.

## Conventions

- **Rules never guess.** A rule that cannot evaluate a resource records a typed skip code
  from `pkg/rules/skip.go`. It does not assume a missing value is zero unless absence
  genuinely means zero, and it never treats a missing price as free.
- **Money is checked against reality.** Every pricing defect found in this project was
  found by comparing a figure against a real invoice, not by a test. If you touch pricing,
  say what you reconciled it against.
- **Comments explain why, not what.** The code says what it does; a comment earns its place
  by recording the reasoning or the failure that shaped it.
- **Determinism.** The same input produces the same output, including ordering. `--at` pins
  the evaluation instant so age-based predicates are reproducible.

## Layout

```
cmd/tellury/       CLI entry point
internal/cli/     command wiring, flags, output selection
pkg/graph/        in-memory resource graph
pkg/cloud/gcp/    Cloud Asset Inventory ingestion and normalization
pkg/metrics/      metric registry and GCP backends
pkg/pricing/      price sources: override, live catalogue, embedded table
pkg/rules/        rule engine, NodeRule interface, skip vocabulary
pkg/rules/gcp/    the shipped rules
pkg/output/       table, JSON, CSV and HTML renderers
```
