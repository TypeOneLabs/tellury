# Contributing to tellury

Contributions are welcome. There's no CLA and nothing to sign — open a pull request.

Rules are the most useful thing you can add. `tellury` ships three, and the interesting ones
are the thousands nobody has written yet: every team knows a way their cloud bill leaks that
no vendor dashboard catches. The rule interface exists to make those easy to write.

## Adding a rule

A rule is a Go package under `pkg/rules/<provider>/<service>/<rule_id>/` implementing the
`rules.Rule` interface. Look at `detached_disk` for the simplest complete example.

What a good rule does:

- **Prices its finding.** A finding without a defensible dollar figure is noise. Show the
  inputs — size, type, region, rate — and let the cost math be checkable.
- **Resolves to true or false.** Thresholds are explicit numbers with a stated reason, not
  tuned constants nobody can justify later.
- **Skips rather than guesses.** If the data needed isn't there, record a skip with a reason.
  `--explain-skips` should tell an operator the difference between "checked, it's fine" and
  "couldn't check". Missing data is never zero.
- **Ships a fixture.** Add a Cloud Asset Inventory JSON fixture that triggers your rule, so
  `tellury scan --fixture` reproduces the finding with no cloud account. This is how reviewers
  verify your work, and it's why offline mode exists.
- **Honours `tellury-exempt=true`** on a resource, checked before anything else.

## Other contributions

Bug reports, docs, a new cloud provider, or a fix from the "Known issues" list in the README
are all just as welcome. For anything large, open an issue first so we can agree on the shape
before you spend the time.

Use the official SDK when talking to a cloud provider — hand-rolling auth, retries, and
pagination is where the bugs live. Other dependencies just need to earn their place.

## Before opening a PR

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

All four should be clean. Add a test for anything you change, and a fixture-based test for a
new rule.

## Licence

`tellury` is [Apache 2.0](LICENSE), and contributions are made under the same licence. That's
all — by opening a PR you're licensing your work under Apache 2.0, same as everything else in
the repository.
